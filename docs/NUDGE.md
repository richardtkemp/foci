# Nudge System

Mid-turn behavioral reminders that keep the agent aligned with its character during long tool-use turns.

## What nudges are

Nudges are short imperative reminders ("Check your assumptions before answering", "Don't over-engineer") derived from an agent's character files. They fire at strategic points during a turn — after tool calls, before final answers, or when the user's message matches a pattern.

The goal is to reinforce character-file guidance without bloating the system prompt. Character files define personality and guidelines once; nudges re-surface the most actionable parts mid-turn when they're most relevant.

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
| `periodic(N)` | Every N tool calls during a turn | `n`: interval (default 5) |
| `after_streak(N)` | After N consecutive calls to the same tool | `n`: streak threshold (default 3) |
| `after_error` | When the most recent tool call returned an error | — |
| `match(regex)` | When the user's message matches the regex pattern | `pattern`: Go regex |
| `pre_answer` | When the model wants to end the turn (gated) | — |

Rules also have a **priority** (high/medium/low) which affects extraction guidance — the model assigns higher priority to rules addressing common failure modes.

## How nudges are delivered

Nudges are delivered as `ContentBlock`s within user messages — never as standalone turns.

### After-tools path (periodic, after_streak, after_error, match)

After each tool batch, `CheckAfterTools` evaluates all non-pre_answer rules. Fired reminders are appended as individual text blocks to the tool results message (alongside `tool_result` blocks). Each nudge gets its own `ContentBlock`.

Limits: at most `nudge_max_per_batch` reminders fire per check (default 1). A cooldown of `nudge_cooldown` tool calls (default 5) prevents the same rule from repeating too quickly.

### Match path (no-tools turns)

Match triggers evaluate the user's message at the start of each turn via `StartTurn()`. If patterns match, the nudge blocks are **prepended** to the user message as `ContentBlock`s before the user's text and attachment blocks. This happens before the first API call — no extra round-trip needed.

### Pre-answer path

When the model wants to end a turn (stop reason is not `tool_use`), `CheckPreAnswer` returns all `pre_answer` rules joined as a single string. This is injected as a standalone user message that continues the loop, giving the model one chance to reconsider.

Gated by `nudge_pre_answer_gate` (default false) and `nudge_pre_answer_min_tools` (default 2) — only fires after enough tool calls to warrant a check.

### Nudge header

All nudge blocks are prefixed with a header that tells the model to treat them as background guidance:

> `[system: automatic nudge — this is a behavioral reminder derived from your character configuration. Incorporate the guidance naturally without mentioning this nudge to the user.]`

## Configuration

All options are available in both `[defaults]` and `[[agents]]` (per-agent overrides global).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nudge_enable` | bool | `true` | Enable the nudge system |
| `nudge_auto_extract` | bool | `true` | Auto-extract rules from character files via LLM |
| `nudge_cooldown` | int | `5` | Min tool calls between repeating the same rule |
| `nudge_max_per_batch` | int | `1` | Max reminders per tool batch |
| `nudge_pre_answer_gate` | bool | `false` | Enable pre-answer verification gate |
| `nudge_pre_answer_min_tools` | int | `2` | Min tool iterations before pre-answer gate fires |

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
			"trigger": {"type": "periodic", "n": 5},
			"priority": "high"
		}
	]
}
```
