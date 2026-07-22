# Nudge System

Mid-turn behavioral reminders that keep the agent aligned with its character during long tool-use turns, plus periodic tool/skill reminders so agents don't forget what's available.

## What nudges are

Nudges are short reminders injected at strategic points during a turn. There are two kinds:

1. **Character nudges** — behavioral reminders ("Check your assumptions before answering", "Don't over-engineer") extracted from character files. They fire after tool calls, before final answers, or when the user's message matches a pattern.
2. **Default nudges** — built-in reminders listing the agent's available tools and skills. They fire every N user turns (default 50) so agents in long conversations don't forget less-used capabilities.

The goal is to reinforce guidance without bloating the system prompt. Character files define personality once; default nudges list capabilities once — both re-surface mid-conversation when most relevant.

## How rules are generated

1. On first session activity (via the `OnActivity` hook), the nudge system checks if rules need extraction.
2. Character files are read and hashed (SHA-256). If the hash matches the existing `nudge-rules.json`, extraction is skipped.
3. When hashes differ (or no rules file exists), the character file contents are sent to the agent's model with the extraction prompt.
4. The model identifies rule-like statements and outputs structured JSON: a terse reminder, the source passage, a trigger type, and a priority level.
5. Rules are saved to `{workspace}/character/nudge-rules.json` (or `{workspace}/nudge-rules.json` if no `character/` directory exists).
6. Re-extraction happens when character files change — detected via content hash during compaction.

When `nudge_auto_extract` is false, the LLM is never called; nudges still fire from an existing rules file.

The extraction prompt asks the model to keep `tool_pattern` (not just `every_n_tools`) frequency-disciplined, and to merge overlapping `tool_pattern`/`regex` rules into one instead of emitting several near-duplicates that would all fire on the same event (#1309) — this is best-effort at generation time; the runtime cross-rule cooldown/cap above is the enforced backstop.

The one-shot extraction call (`ExtractViaRunOnce`, used by delegated agents) uses `nudge_extraction_model` when set; otherwise it falls back to whatever model the runner defaults to.

## Trigger types

Each rule has one trigger that determines when it fires:

| Trigger | When it fires | Parameters |
|---------|--------------|------------|
| `every_n_tools(N)` | Every N individual tool calls during a turn | `n`: interval (default 5) |
| `every_n_turns(N)` | Every N user turns (lifetime, never reset) | `n`: interval (default 50) |
| `after_error` | When the most recent tool call returned an error | — |
| `tool_pattern` | When the recent tool call(s) match a tool-name regex and/or a tool-input regex | `tool_pattern`, `input_pattern`, `consecutive` (default 1) |
| `regex(pattern)` | When the user's message matches the regex pattern | `pattern`: Go regex |
| `pre_answer` | When the model wants to end the turn (gated) | — |

Rules also have a **priority** (high/medium/low) which affects extraction guidance — the model assigns higher priority to rules addressing common failure modes.

## How nudges are delivered

Nudges are delivered as `ContentBlock`s within user messages — never as standalone turns.

### After-tools path (every_n_tools, after_error, tool_pattern)

After each tool batch, `CheckAfterTools` evaluates all non-pre_answer, non-regex rules. Fired reminders are appended as individual text blocks to the tool results message (alongside `tool_result` blocks). Each nudge gets its own `ContentBlock`.

Limits: at most `nudge_max_per_batch` reminders fire per check (default 1). A cooldown of `nudge_cooldown` tool calls (default 5) prevents the same rule from repeating too quickly.

**Cross-rule `tool_pattern` cooldown (#1309).** The per-rule cooldown above only throttles one specific rule repeating — it does nothing to stop several near-duplicate `tool_pattern` rules (e.g. multiple independently-extracted "security" passages that all match `Edit`/`Write`) from round-robining: each individual rule respects its own cooldown, but a sibling rule is free to fire on the very next matching tool call, so the group as a whole nagged on nearly every edit (proven 2026-07-16: ~77 firings/day from one such quartet). `Scheduler` additionally tracks the last-fired tool count *per trigger type* (`lastFiredByType`) and applies the same `nudge_cooldown` window across every `tool_pattern` rule collectively, not just within one rule. `every_n_tools`/`after_error` are not grouped this way — they already have their own frequency discipline (`N`, and `nudge_max_per_batch` respectively) and are usually deliberately distinct concerns rather than near-duplicates.

**`after_error` benign-exit exemption (#1309).** CC's Bash tool result sets `is_error` purely from the shell exit code, so a `grep`/`test`/`[` invocation that legitimately finds nothing (exit 1) is indistinguishable from a real failure — this false-positived the "check errors" nudge at a ~25x/day, 100%-false-positive rate. `shouldFire`'s `after_error` case now checks the most recently recorded tool call (via the same ring buffer `tool_pattern` uses) and skips firing when it's a `Bash` call whose command starts with a known match/test-style tool (`grep`, `egrep`, `fgrep`, `rg`, `ack`, `test`, `[`).

### Regex path (no-tools turns)

Regex triggers evaluate the user's message at the start of each turn via `StartTurn()`. If patterns match, the nudge blocks are **prepended** to the user message as `ContentBlock`s before the user's text and attachment blocks. This happens before the first API call — no extra round-trip needed.

Also capped at `nudge_max_per_batch` (#1309) — a single message can match several independent regex rules at once; without the cap every one of them stacked into the same turn-start injection. Matches beyond the cap are skipped for that turn rather than deferred (the same message won't recur once the turn ends).

### Pre-answer path

When the model wants to end a turn (stop reason is not `tool_use`), `CheckPreAnswer` returns all `pre_answer` rules joined as a single string. This is injected as a standalone user message that continues the loop, giving the model one chance to reconsider.

Gated by `nudge_pre_answer_gate` (default false) and `nudge_pre_answer_min_tools` (default 2) — only fires after enough tool calls to warrant a check. Not subject to `nudge_max_per_turn` (below) — it's a single, deliberate end-of-turn check, not ambient reminder noise.

### Every-N-turns path (default nudges)

Default tool/skill reminder nudges use the `every_n_turns` trigger, which fires every N user turns (default 50). The turn counter is a lifetime counter — it increments on every `StartTurn()` call and is never reset. Fired reminders are **prepended** to the user message as `ContentBlock`s (same injection point as regex triggers).

Only tools and skills actually registered for the agent appear in the reminder.

### Nudge header

Nudges are wrapped in a `<system-reminder>` block — mirroring the same wrapper Claude Code itself uses for injected context (e.g. environment/date info) — so the model applies its native low-priority-context handling rather than treating the reminder as user input:

> `<system-reminder>`
> `This is a background nudge for you to weigh — a private note to yourself, not a message from the user.`
> `{reminder text}`
> `IMPORTANT: this nudge may or may not be relevant to what you're doing — apply it only if it genuinely fits, and don't refer to it directly in your reply. ...`
> `</system-reminder>`

The opening preamble (`nudge-preamble.md`) and the closing line (`nudge-user-boundary.md` for the bundled path, `nudge-reply-instruction.md` for the standalone path) are per-agent-overridable prompts, resolved the same way as the compaction-summary prompt. When several nudges are bundled into the same turn (e.g. a regex trigger and a turn-interval trigger firing together), only the first reminder opens the tag and only the closing delimiter closes it, so the region stays a single well-formed block instead of nesting.

## Configuration

All options are available in both `[nudge]` (global) and `[[agents]].nudge.*` (per-agent overrides global).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nudge_enable` | bool | `true` | Enable the nudge system |
| `nudge_auto_extract` | bool | `true` | Auto-extract rules from character files via LLM |
| `nudge_cooldown` | int | `5` | Min tool calls between repeating the same rule (also the shared cross-rule window for `tool_pattern`, #1309) |
| `nudge_max_per_batch` | int | `1` | Max reminders per tool batch (also caps simultaneous regex matches, #1309) |
| `nudge_max_per_turn` | int | `0` | Max total reminders injected across a whole turn, summed over every trigger. `0` = unlimited (#1309) |
| `nudge_extraction_model` | string | `""` | Model for the one-shot nudge-extraction LLM call. `""` uses the backend's own cheap-batch default (currently `sonnet`, #1309) |
| `nudge_pre_answer_gate` | bool | `false` | Enable pre-answer verification gate |
| `nudge_pre_answer_min_tools` | int | `2` | Min tool iterations before pre-answer gate fires |
| `nudge_default_enable` | bool | `true` | Enable built-in tool/skill reminders |
| `nudge_default_frequency` | int | `50` | Turns between tool/skill reminders |

## Rules file format

`nudge-rules.json` is a JSON file with a content hash and an array of rules:

```json
{
	"content_hash": "sha256hex...",
	"rules": [
		{
			"text": "Check assumptions before answering — don't guess.",
			"source_file": "CRAFT.md",
			"source_text": "Always verify your assumptions...",
			"trigger": {"type": "every_n_tools", "n": 5},
			"priority": "high"
		}
	]
}
```
