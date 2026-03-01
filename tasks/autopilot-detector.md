# Autopilot Detector

## Problem

Agents fall into "autopilot" — rapid sequential tool calls without pausing to verify they're doing what the user actually asked. This causes wasted mana and wrong actions.

## Solution

Count consecutive tool-call loops without an intervening user message. When the count exceeds a threshold, inject a system warning into the conversation.

## Config

```toml
[defaults]
autopilot_threshold = 10           # tool calls before warning (0 = disabled)
autopilot_prompt = "You've made many tool calls without hearing from the user. Are you on autopilot? Stop and check: is what you're doing right now what they actually asked for?"

[[agents]]
id = "clutch"
autopilot_threshold = 10
autopilot_prompt = "..."           # override per agent
```

- **autopilot_threshold** (int, default 10) — number of consecutive tool calls before injecting the warning. 0 disables.
- **autopilot_prompt** (string) — the warning text injected as a system message.
- Available at `[defaults]` and per-agent. Per-agent overrides defaults.

## Behaviour

- Counter increments each tool-call loop (one "loop" = agent calls tool(s) and gets results).
- Counter resets to 0 on each inbound user message.
- When counter hits threshold, inject `autopilot_prompt` as a system-role message in the next turn.
- Only inject once per threshold crossing (don't repeat every loop after). Reset the injection flag when counter resets.

## Not in scope

- Blocking tool calls
- Requiring user confirmation
- Any UI changes
