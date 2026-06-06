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
6. Re-extraction happens when character files change — detected via content hash on `/reload` or during compaction.

When `nudge_auto_extract` is false, the LLM is never called; nudges still fire from an existing rules file.

## Trigger types

Each rule has one trigger that determines when it fires:

| Trigger | When it fires | Parameters |
|---------|--------------|------------|
| `every_n_tools(N)` | Every N individual tool calls during a turn | `n`: interval (default 5) |
| `every_n_turns(N)` | Every N user turns (lifetime, never reset) | `n`: interval (default 50) |
| `after_error` | When the most recent tool call returned an error | — |
| `regex(pattern)` | When the user's message matches the regex pattern | `pattern`: Go regex |
| `pre_answer` | When the model wants to end the turn (gated) | — |

Rules also have a **priority** (high/medium/low) which affects extraction guidance — the model assigns higher priority to rules addressing common failure modes.

## How nudges are delivered

Nudges are delivered as `ContentBlock`s within user messages — never as standalone turns.

### After-tools path (every_n_tools, after_error, regex)

After each tool batch, `CheckAfterTools` evaluates all non-pre_answer rules. Fired reminders are appended as individual text blocks to the tool results message (alongside `tool_result` blocks). Each nudge gets its own `ContentBlock`.

Limits: at most `nudge_max_per_batch` reminders fire per check (default 1). A cooldown of `nudge_cooldown` tool calls (default 5) prevents the same rule from repeating too quickly.

### Regex path (no-tools turns)

Regex triggers evaluate the user's message at the start of each turn via `StartTurn()`. If patterns match, the nudge blocks are **prepended** to the user message as `ContentBlock`s before the user's text and attachment blocks. This happens before the first API call — no extra round-trip needed.

### Pre-answer path

When the model wants to end a turn (stop reason is not `tool_use`), `CheckPreAnswer` returns all `pre_answer` rules joined as a single string. This is injected as a standalone user message that continues the loop, giving the model one chance to reconsider.

Gated by `nudge_pre_answer_gate` (default false) and `nudge_pre_answer_min_tools` (default 2) — only fires after enough tool calls to warrant a check.

### Every-N-turns path (default nudges)

Default tool/skill reminder nudges use the `every_n_turns` trigger, which fires every N user turns (default 50). The turn counter is a lifetime counter — it increments on every `StartTurn()` call and is never reset. Fired reminders are **prepended** to the user message as `ContentBlock`s (same injection point as regex triggers).

Only tools and skills actually registered for the agent appear in the reminder.

### Nudge header

All nudge blocks are prefixed with a header that tells the model to treat them as background guidance:

> `[Background nudge — a private note to yourself, not from the user. Apply it only if it genuinely fits what you're already doing; if it doesn't, ignore it and don't bend your reply to accommodate it. Don't refer to the nudge directly in what you send.]`

## Configuration

All options are available in both `[nudge]` (global) and `[[agents]].nudge.*` (per-agent overrides global).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nudge_enable` | bool | `true` | Enable the nudge system |
| `nudge_auto_extract` | bool | `true` | Auto-extract rules from character files via LLM |
| `nudge_cooldown` | int | `5` | Min tool calls between repeating the same rule |
| `nudge_max_per_batch` | int | `1` | Max reminders per tool batch |
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
