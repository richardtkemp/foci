<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Prompts, turns & injections

How foci builds what you see each turn, where its prompt templates live, and how to customise them.

## Prompt files: where they live & how to customise them

Foci drives several **system-generated turns** from prompt template files — one file per turn type:

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

The shipped defaults are seeded into `~/shared/prompts/` on first start (seed-if-missing — your edits there are never overwritten by a later start). **Two ways to customise a prompt for your purposes:**

1. **Copy it into your workspace prompts dir.** Drop a file with the matching filename into `<workspace>/prompts/`. When a turn resolves its prompt, foci searches `<workspace>/prompts/` first, then `~/shared/prompts/`, then falls back to the embedded default — so a same-named file in your workspace wins. This keeps the customisation per-agent and in your own tree.
2. **Point config at an explicit path.** Each turn type has a config key (e.g. the keepalive `prompt`, reflection `interval_prompt` / `session_end_prompt` / `compaction_prompt`, `consolidation_prompt`, `compaction_summary_prompt`). Set it to a file path to use that file. Special values: unset or `"default"` → the search-then-embedded behaviour above; `"none"` → that turn is **disabled** entirely.

So: tweak wording → copy into `<workspace>/prompts/`; disable a turn → set its config key to `"none"`; point at a shared file elsewhere → set the key to its path.

The **character files** that make up your identity are separate from these turn templates — they load from your `character/` dir and are reloaded on compaction and restart, so editing them takes effect on the next rebuild.

## The `[meta]` header

Every inbound message carries a `[meta]` line:

- `time` — RFC3339 receipt timestamp (the user's send time).
- `gap` — time since the previous message (`none` on first).
- `model` — the model for this turn (developer prefix stripped).
- `via` — delivery channel: `telegram`, `android`, `voice`, `api`, or `cron` for system-initiated turns (keepalive, reflection, scheduled wake), `async` for async tool results, `tmux` for watch notifications.
- `mana` — remaining quota % + 🟢/🔴 indicator (omitted if no data).

## `[state]` and `[reminders]`

- `[state]` — a dashboard line (todos open/high, tasks, scratchpad entries) injected as context.
- `[reminders]` — due reminders, surfaced once then auto-dismissed; only on root sessions, not branches.

## System injections

Host-initiated events arrive as a **user-role message** wrapped with `[SYSTEM INJECTION — …]`. The wrapper tells you: this was sent by the host, the user hasn't seen it; either actively tell the user about it or, if they needn't know, respond `[[NO_RESPONSE]]`. Includes the event's timestamp. Examples: proactive warnings, system update notices.

## Nudges

Behavioural reminders extracted automatically from your character files and prepended to the user message. Trigger types: `every_n_tools`, `pre_answer`, `after_error`, `regex`, `tool_pattern`. They are **heuristic, not corrective** — they fire on patterns, not on whether the guidance applies. If a nudge doesn't fit what you're doing, ignore it and respond `[[NO_RESPONSE]]` (when it's the whole turn) rather than bending your reply to it. Nudges are suppressed on system-internal turns (reflection, consolidation, session-end memory).

Auto-extraction is controlled by `nudge_auto_extract` (default on, in the `[nudge]` config). Set it to `false` to stop the LLM re-deriving rules from your character files and instead manage them by hand in `nudge-rules.json` (in your character/workspace dir). `nudge_enable` is the master on/off for the whole system.

## `[[NO_RESPONSE]]`

The silence sentinel. When it is the **entire** message, the turn completes without delivering anything to the user. Any text before or after it still ships — so on a no-op turn, emit the bare sentinel with nothing else. May be wrapped in markdown (backticks/bold) and still counts.

## Compaction

When a session's context grows past its threshold, foci compacts: it summarises the conversation (via `compaction-summary.md`), optionally rotates to a fresh session key, fires the memory hooks, then reloads character files. Triggers: the main token threshold, an optional pre-mana-refresh compaction (start a fresh budget window with a smaller context), or manual `/compact`. A post-compaction hand-off message (`compaction-handoff.md`) gives the next context continuity.

The periodic memory turns (reflection, consolidation) and their scheduling live in **scheduled-tasks.md**.