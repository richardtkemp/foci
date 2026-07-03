---
name: foci-development
description: Developing the foci platform itself (Go server + backends). Architecture, the CC/opencode backends, the routing/delivery model, and the turn/steer/ask lifecycle — the internals you need when CODING foci, not when operating as an agent. Read the relevant subfile before changing that area.
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
