---
name: foci-development
description: Developing the foci platform itself (Go server + backends). Landing/deploy/rollback rules, running tests, architecture, the CC/opencode backends, routing/delivery, and the turn/steer/ask lifecycle. Read the relevant subfile before changing that area.
owner: foci
seeded: true
---

# Foci Development — Coding the Platform

For working ON foci's Go codebase (`/home/rich/git/foci`, public `github.com/richardtkemp/foci`). Operating as an agent is `foci-usage`; investigating a running instance is `foci-debugging`.

> **This `SKILL.md` is yours to customise** (seed-if-missing). The subfiles below **ship with foci and are overwritten on restart**: edit them in the foci repo (`shared/skills/foci-development/`), not in `~/shared/skills/`.

**Read `docs/WIRING.md` FIRST for any foci investigation** (startup, packages, callbacks, dispatch, timers). Update it when you REWIRE (new callback/hook/flow/timer/package). `SPEC.md` = design intent.

## Where to look

| Subfile | Read it when you touch… |
|---|---|
| **architecture.md** | CC vs opencode backends, session-key grammar, agent-id defaults, cross-backend hooks. |
| **backends.md** | CC cold-launch flags, idle timeouts, the opencode HTTP contract + session scoping, sqlite DSN pragmas. |
| **routing.md** | Outbound delivery: `internal/route` (`ConnFor`, policies, outcomes, `Broadcast`), `send_to_session`, agent-initiated messages. |
| **turns.md** | Steer vs SourceUser, `foci_ask`, app-vs-typed ask-capture gates, CC delivery tracking. |

> **A new server↔app FAP wire frame** (`internal/fap/`) is cross-repo: Go and foci-client Kotlin must stay byte-compatible. Follow the **foci-client-dev** skill's `add-fap-frame.md` (it includes the Go steps).

## Landing & deploy invariants

- **main is reached ONLY through `make land`, on every host, the work Mac included.** Never `git push` to main, never `push <branch>:main`, never ff local main to a branch and push it, not even for a one-liner or a `scripts/sync-modelinfo` branch. A direct push skips the test gate (#2583: a Mac sync broke every build). Mac agents may land (Dick, 2026-10-28), only with `make land` from their own feature-branch worktree. It needs `flock` on PATH (`make macos-deps`; docs/INSTALL.md "Landing on a Mac") and fails closed with `LAND_RESULT: FAILED ...` without the lock.
- **main is unit-green by invariant.** `make land` rebases onto origin/main, re-runs the unit suite on the combined result, then pushes `HEAD:main`. So a red **unit** test on a fresh feature branch is yours until the base commit proves otherwise. **Integration greenness is NOT guaranteed**: verify it separately when it matters.
- **Deploys do not prove they run origin/main.** `deploy`/`update` run `sync-main` (`git merge --ff-only origin/main`) and print `deploying <sha> (== origin/main)`. When local main is *ahead*, ff-only is a no-op, so it builds **unpushed local commits** and still prints that line (six un-landed commits reached production this way, 2026-07-26). To know what runs, check the live binary against `origin/main`, `git log origin/main..main` (unpushed = suspect), and the binary's mtime. (Land mechanics: `writing-code`/`git-worktree.md`.)

### Rolling back a deploy to last-known-good

`make update` refuses a detached HEAD (*"deploy must run on main (currently on HEAD)"*), so you cannot deploy a throwaway worktree at the good commit. `sync-main` does not rewind main either (ff-only hole above). So you must move local `main`. Both preconditions are load-bearing:

```sh
git -C /home/rich/git/foci status --porcelain          # MUST be empty: reset --hard would destroy a peer's WIP
for c in <each sha being rewound>; do                  # every rewound commit must survive on another branch
  git -C /home/rich/git/foci branch --contains $c | grep -v '^\*\? *main$'
done                                                   # no other branch holds it = you orphan work
git -C /home/rich/git/foci reset --hard <lastgood>     # e.g. origin/main
sudo make -C /home/rich/git/foci update                # then END YOUR TURN IMMEDIATELY
```

The changelog line `foci: changelog staged (<old> -> <new>)` confirms the direction. A rollback reverts code, not data: check for residue the bad build left (e.g. rows in `~/data/state.db`).

## Verifying a change

**Bare `go build`/`go test`/`go vet` are denied (Dick, 2026-09-23)**, including `/usr/bin/go …` and `go -C <dir> …`. Use `make build` (foci-gw), `make vet` (type-checks every package) and `make test-one`. All take the `/tmp/heavy` lock. `go doc`, `go list`, `go env` and `go version` still run.

**Run ONE package with `make test-one PKG=./internal/<pkg>/ [RUN=<Name>] [COUNT=N]`.** `make test` has no package filter, and a bare `go test` panics (packages need the sandboxed HOME). `test-one` uses the exact env of `make test` (`scripts/seal-test.sh`). Never hand-roll that env: a missing var looks like a test failure or a "confirmed" flake (#1709). Every run is `go test -v` (`V=1` does nothing, #2348). `COUNT=N` is for flake checks.

**`test-one`, `vet`, `test` and `lint` run on a Mac when one is reachable (#2525, #2526, #2544).** `.remote.mk` lists build hosts (`REMOTE_HOSTS`, in order, with or without a build lock). The bare targets rsync the checkout to the first host that answers and run there (darwin build tags), else run here. The first output line says which (`>>> test-one: running on build host pro: …` / `running LOCALLY (<why>)`). The exit code is the Mac's; its log is copied back (path printed). `local-<target>` forces this host, `remote-*` forces a Mac, `REMOTE_PICK=<name>` picks one. So `make land`'s gate runs on the Mac too: there `test` first runs `GOOS=linux go vet ./...`, and `lint` analyses the Linux build with this host's Go. CI tests main on Linux. Linux-only behaviour (procx caps, Landlock sealing, `_linux.go`) needs `local-test-one` / `local-test`. `build` and `integration` always run locally (`mk/remote.mk` says why).

**Tests see a fixed environment, not your shell's (#2348).** `seal-test.sh` starts them from `env -i` with a pinned system `PATH`, the sandbox dirs, Go caches, the caller's `GO*`/`CGO_*`, and `OPTIN_VARS`. So a tool a test needs must be in a system dir (or come through an opt-in var, like `FOCI_OPENCODE_BIN`), and a new env var a test reads from the caller must be added to `OPTIN_VARS` or it never arrives. Details: `foci-debugging/test-harness.md`.

**You cannot sandbox a `foci` CLI probe.** The CLI prefers the gateway's unix socket, so `FOCI_ADDR` is ignored when you run as an agent. A `foci send` "as a test" is delivered for real.

**A `cmd/foci-gw` test cannot reproduce a delegated-transport bug.** `httpTestSetup` builds an API-transport agent. Where the transports differ (e.g. `LogConversationSent` is real on one, a no-op on the other), the test passes for the wrong reason. Run the fail-arm before you believe it.
