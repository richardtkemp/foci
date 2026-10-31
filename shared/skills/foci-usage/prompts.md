<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Prompts, turns & injections

## Prompt files: where they live & how to customise them

Each **system-generated turn** type has a prompt template file:

| Filename | Drives… |
|----------|---------|
| `keepalive.md` | the keepalive (cache-warm / liveness) turn |
| `background.md` | the idle background-work turn |
| `reflection.md` | reflection / memory-formation turns (interval, session-end, post-compaction) |
| `memory-consolidation.md` | the consolidation turn that curates `MEMORY.md` |
| `compaction-summary.md` | the summary written when a session compacts |
| `compaction-handoff.md` | the continuity hand-off injected after compaction |
| `branch-orientation-headless.md` / `branch-orientation-facet.md` | orientation text for branch sessions (cron, spawn) |
| `weekly-character-review.md` | the weekly character-review turn |

The shipped defaults seed into `~/shared/prompts/` if missing (your edits there persist). **Two ways to customise:**

1. **Per-agent override:** put a same-named file in `<workspace>/prompts/`. foci searches `<workspace>/prompts/`, then `~/shared/prompts/`, then the embedded default.
2. **Config path:** *most* turn types have a config key (keepalive `prompt`; reflection `interval_prompt` / `session_end_prompt` / `compaction_prompt`; `consolidation_prompt`). Set it to a file path. Unset or `"default"` → the search above; `"none"` → that turn is **disabled**.

`compaction-summary.md` (and the nudge framing prompts) are **file-only** — they have no config key, so they're customised by method 1 only. Disable the compaction summary prompt by dropping an *empty* `compaction-summary.md` in `<workspace>/prompts/` (the file-only equivalent of `"none"`).

The **character files** that make up your identity are separate from these turn templates — they load from your `character/` dir and are reloaded on compaction and restart, so editing them takes effect on the next rebuild.

## The `[meta]` / `[state]` / `[ask]` header (the statusline)

Every inbound message carries a header built from a configurable **statusline template** (`internal/agent/statusline.go`). The default template is `[meta] time={time} gap={gap} model={model} via={via}` / `[state] {state}` / `[ask] {ask}`; a line whose only placeholders all render empty is dropped entirely, so on a fresh session with nothing to report only `[meta]` appears.

- `time` — RFC3339 receipt timestamp (the user's send time).
- `gap` — time since the previous message (`none` on first).
- `model` — the model for this turn (developer prefix stripped).
- `via` — delivery channel: `telegram`, `android`, `voice`, `api`, or `cron` for system-initiated turns (keepalive, reflection, scheduled wake), `async` for async tool results, `tmux` for watch notifications.
- `[state]` — a dashboard line (todos open/high, tasks, scratchpad entries), self-omits when every store is empty.
- `[ask]` — appears only while a `foci_ask` is paused (`/pause`): names the paused request id as a reminder that plain-text replies are routing to you as normal turns rather than answering it (`/resume` to restore).

**`prev_cost` / `prev_tokens` are deliberately NOT in the default**: a per-turn cost figure made agents ration their own budget. They still exist as `{cost}`/`{tokens}` fields (plus bare `{cost_raw}`/`{tokens_in}`/`{tokens_out}`/`{cache_read}`/`{cache_write}`) for anyone who opts in via a custom `statusline` config (`docs/CONFIG.md`). There is no `mana`/quota field in the statusline — check remaining quota via the `/mana` command (see the `mana` skill) instead; it exists only for agents on a Claude Code backend.

## `[reminders]`

Due reminders, surfaced once then auto-dismissed; only on root sessions, not branches. Separate from the statusline above — injected as its own block (`internal/agent/system_blocks.go`).

## System injections

Host-initiated events arrive as a **user-role message** wrapped with `[SYSTEM INJECTION — …]`. The user has not seen it: tell the user about it, or respond `[[NO_RESPONSE]]` if they need not know. Examples: proactive warnings, system update notices.

## Nudges

Behavioural reminders extracted automatically from your character files and prepended to the user message. Trigger types: `every_n_tools`, `pre_answer`, `after_error`, `regex`, `tool_pattern`. They fire on patterns, not on whether the guidance applies: if a nudge does not fit, ignore it (respond `[[NO_RESPONSE]]` when it is the whole turn) rather than bend your reply to it. Nudges are suppressed on system-internal turns (reflection, consolidation, session-end memory).

Auto-extraction is controlled by `nudge_auto_extract` (default on, in the `[nudge]` config). `false` stops re-deriving rules from your character files; you then manage them by hand in `nudge-rules.json` (character/workspace dir). `nudge_enable` is the master on/off for the whole system.

## `[[NO_RESPONSE]]`

The silence sentinel. When it is the **entire** message, the turn completes without delivering anything to the user. Any text before or after it still ships — so on a no-op turn, emit the bare sentinel with nothing else. May be wrapped in markdown (backticks/bold) and still counts.

## Compaction

foci summarises the conversation (`compaction-summary.md`), optionally rotates the session key, fires the memory hooks, reloads character files, then injects a hand-off (`compaction-handoff.md`). Triggers: the token threshold, an optional pre-mana-refresh compaction (a smaller context for the fresh budget window), or `/compact`.

The periodic memory turns (reflection, consolidation) and their scheduling live in **scheduled-tasks.md**.