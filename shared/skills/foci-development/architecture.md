<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Architecture

## Agent / backend model

Foci bridges a messaging channel (Telegram, Discord, HTTP API, voice, the native app) to an **LLM backend**, and gives the agent tools, persistent memory and a turn lifecycle. Two backends:

- **Claude Code (CC)**: most agents. Foci drives a `claude` subprocess (delegated backend). CC does inference and its own tools; foci owns platform, session and turn plumbing. **A pure-CC deployment needs NO Anthropic API key** (CC uses `~/.claude/.credentials.json`). A startup `no Anthropic credentials` line is a caching probe, not an inference failure.
- **opencode**: e.g. arnix (glm). Foci talks HTTP to a local `opencode serve`. See **backends.md**.

## Session keys are stable identities

`{agentID}/{type}{id}[/{childType}{childTS}]` (no `versionTS` segment since the 2026-07 refactor):

```
main/c123              chat session (c = chat, id = chatID)
main/iwork             independent session (i = independent, id = name/epoch)
main/c123/b1709596800  branch of a chat session (b = branch)
```

- Chat keys are **deterministic** (`agent/c<chatID>`). `chat_metadata` only registers platform *ownership*; it does not persist keys.
- **Compaction and /reset archive the live file in place** (`root.jsonl → root.<ts>.jsonl`). The key never changes, so there is no rotation machinery (`RotateKey`, `SessionKeyBase`, `SessionInFlightKey` are gone).
- `session.ParseSessionKey` is the single parser. The session index has real `agent_id` / `chat_id` / `is_root` columns: never rebuild the grammar with SQL `LIKE`.

## Agent-id smart defaults

Most per-agent config derives from the **agent ID** (e.g. clutch's workspace, character dir and data dir). Override only the non-defaults.

## Cross-backend behaviour

When backends differ in mechanism for one outcome, hook where they already converge, not with N per-backend hooks. Example: an opencode 404 and a CLI non-zero exit on a stale resume both reach `DelegatedManager`'s retry-without-resume path, so one hook covers both.
