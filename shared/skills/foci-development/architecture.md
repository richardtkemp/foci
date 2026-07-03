<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Architecture

## Agent / backend model

Foci bridges a messaging channel (Telegram, Discord, HTTP API, voice) to an **LLM backend** and gives an agent tools, persistent memory, and a turn lifecycle. The two backends:

- **Claude Code (CC)** — most agents. Foci drives a `claude` subprocess (delegated backend); CC handles inference + its own tools, foci owns the platform/session/turn plumbing. **A pure-CC deployment needs NO Anthropic API key** — CC uses its own `~/.claude/.credentials.json`. So a startup `no Anthropic credentials` line is a *caching probe*, not an inference failure.
- **opencode** — e.g. arnix (glm). Foci talks to a local `opencode serve` over HTTP. See **backends.md**.

## Session keys — stable identities

A session key is `{agentID}/{type}{id}[/{childType}{childTS}]` — and it is a **stable identity** (the `versionTS` segment was removed in the 2026-07 "stable session keys" refactor):

```
main/c123              chat session (c = chat, id = chatID)
main/iwork             independent session (i = independent, id = name/epoch)
main/c123/b1709596800  branch of a chat session (b = branch)
```

- Chat keys are **deterministic** (`agent/c<chatID>`), so `chat_metadata` no longer persists keys — it just registers platform *ownership*.
- **Compaction and /reset archive the live file in place** (`root.jsonl → root.<ts>.jsonl`) instead of minting a new key. The key never changes across a session's life. (This deleted the whole rotation-migration machinery — `RotateKey`, `SessionKeyBase`, `SessionInFlightKey`, etc.: the key IS the identity.)
- `session.ParseSessionKey` is the single parser; the session index has real `agent_id` / `chat_id` / `is_root` columns (no SQL `LIKE` grammar reconstruction).

## Agent-id smart defaults

Most per-agent config derives from the **agent ID** — only override the non-defaults. (E.g. clutch's workspace, character dir, data dir all derive from `clutch`.)

## Cross-backend behaviour

When two backends differ in *mechanism* for the same outcome, **find where they already converge** and hook there rather than bolting on N parallel per-backend hooks. (E.g. an opencode 404 and a CLI non-zero exit on a stale resume both land in `DelegatedManager`'s retry-without-resume path — one hook covers both.)

## Docs

`docs/WIRING.md` is the wiring map (startup, packages, callbacks, dispatch, timers) — consult first, and update it whenever you rewire. `SPEC.md` is design intent.
