---
name: foci-development
description: Developing the foci platform itself (Go server + backends). Architecture, the CC/opencode backends, the routing/delivery model, and the turn/steer/ask lifecycle — the internals you need when CODING foci, not when operating as an agent. Read the relevant subfile before changing that area.
owner: foci
seeded: true
---

# Foci Development — Coding the Platform

For working ON foci's Go codebase (`/home/rich/git/foci`, public `github.com/richardtkemp/foci`). This is *developer* knowledge — the internals — distinct from `foci-usage` (operating as an agent) and `foci-debugging` (investigating a running instance).

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/foci-development/`), not the deployed `~/shared/skills/` copy.

**Consult `docs/WIRING.md` FIRST for any foci investigation** (startup, packages, callbacks, dispatch, timers); update it whenever you REWIRE (new callback/hook/flow/timer/package). `SPEC.md` = design intent.

## Where to look

| Subfile | Read it when you're touching… |
|---|---|
| **architecture.md** | The big picture: CC vs opencode backends, the agent/session model, session-key grammar, agent-id smart defaults. |
| **backends.md** | Backend-specific wiring: CC cold-launch flags, the CC shell-tool set, idle timeouts, the opencode HTTP contract + session scoping, sqlite DSN pragmas. |
| **routing.md** | Outbound delivery: the `internal/route` cascade (`ConnFor`, policies, outcomes, `Broadcast`), `send_to_session`, and how an agent-initiated/unsolicited message reaches a chat. |
| **turns.md** | The turn lifecycle: steer vs SourceUser folding, `foci_ask` (async, persistence), and the app-vs-typed ask-capture gates. |

> **Adding a server↔app FAP wire frame** (a new WebSocket frame in `internal/app/fap/`) is a cross-repo task — the Go server half **and** the foci-client Kotlin half must stay byte-compatible. The full end-to-end chain lives in the **foci-client-dev** skill's `add-fap-frame.md` (it includes the Go steps); use it rather than reconstructing the sequence here.

## Landing & deploy invariants (post-`make land`, #1448)

Two facts about this repo that only became true once the land-script + deploy-from-origin pieces landed (2026-07-21). They set expectations — hold them so peer churn doesn't read as breakage:

- **main is unit-green by invariant.** `make land` rebases onto origin/main, re-runs the unit suite on the *combined* result, and only then pushes `HEAD:main`. So a red **unit** test on a fresh feature branch is **mine** until the base commit proves otherwise — don't reflexively blame a peer's landing. The invariant covers unit tests only: **integration greenness is NOT guaranteed** — don't expect it, and verify integration separately when it matters.
- **deploys INTEND to track origin/main — but the guard only enforces "not behind", not "equal" (verified 2026-07-27).** `deploy`/`update` run `sync-main`, which does `git merge --ff-only origin/main` and then prints `deploying <sha> (== origin/main)`. **That message can lie.** `--ff-only` is a no-op "Already up to date" when local main is *ahead*, so it does NOT rewind — it cheerfully builds and installs **unpushed local commits** while asserting they are origin/main. This is not hypothetical: six un-landed codex commits reached production this way on 2026-07-26 (manual `merge --ff-only` into main at 23:50, `make update` at 23:51). So: **never infer what's running from that log line.** Verify the live binary against `origin/main` **and** `git log origin/main..main` (unpushed = suspect) **and** the binary's mtime. (Mechanics of the land flow itself: `writing-code`/`git-worktree.md` + MEMORY "Concurrent Sessions & Deploys".)

### Rolling back a deploy to last-known-good

Two walls make the obvious approach fail, in this order:

1. **`make update` refuses a detached HEAD** — `sync-main` aborts with *"deploy must run on main (currently on HEAD)"*. So you **cannot** build a clean throwaway worktree at the good commit and deploy that, which is the natural first instinct.
2. Because of the ff-only hole above, `sync-main` will **not** rewind local main for you either.

So deploying an *older* commit means actually moving local `main` to it. Preconditions first — both are load-bearing:

```sh
git -C /home/rich/git/foci status --porcelain          # MUST be empty: reset --hard would destroy a peer's WIP
for c in <each sha being rewound>; do                  # every rewound commit must survive on another branch
  git -C /home/rich/git/foci branch --contains $c | grep -v '^\*\? *main$'
done
git -C /home/rich/git/foci reset --hard <lastgood>     # e.g. origin/main
sudo make -C /home/rich/git/foci update                # then END YOUR TURN IMMEDIATELY
```

Gotchas: the changelog line (`foci: changelog staged (<old> -> <new>)`) is the cheapest confirmation the rollback direction was right. Afterwards, check whether the bad build left **residue** the rollback can't undo (it reverts code, not data) — e.g. rows written to `~/data/state.db` while it ran. And `git branch --contains` before resetting is the whole safety argument: if a sibling branch descends from the commits you're rewinding, they survive the reset; if none does, you are about to orphan work.

## Verifying a change (two traps)

**Running ONE package.** `make test` has no package filter and bare `go test ./<pkg>/` panics — several packages need the sandboxed HOME the Makefile sets. Replicate it:

```
T=/tmp/fgw/test-$(date +%s); mkdir -p $T/home
HOME=$T/home TMPDIR=$T FOCI_TMPDIR=$T FOCI_TEST_TMPDIR=$T go test ./internal/<pkg>/ -run <Name> -count=1
```

That sandboxed HOME also strips git's `safe.directory`, so anything invoking `go build` with VCS stamping fails as "dubious ownership" — add `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=<repo>`.

**You cannot sandbox a `foci` CLI probe.** The CLI prefers the gateway's unix socket, so `FOCI_ADDR` is never read when you run as an agent — pointing it at a dead port does nothing and the command really executes. A `foci send` you run "as a test" is delivered for real.
