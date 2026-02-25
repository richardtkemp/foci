# Task: Per-agent config for tool call visibility in Telegram (#80)

## Problem

Currently tool calls are shown in Telegram for all agents. Some agents (especially user-facing ones like Fotini) shouldn't show tool use messages — it's confusing for non-technical users.

## Desired Behaviour

New config option `show_tool_calls` (boolean) that controls whether tool use messages appear in Telegram.

**Config (global and per-agent):**
```toml
[telegram]
show_tool_calls = true  # global default

[[agents]]
id = "fotini"
show_tool_calls = false  # hide tool calls for this agent
```

Per-agent overrides global. Default: true (current behaviour).

## Implementation

- Find where tool call messages are sent to Telegram (likely in the agent loop or telegram/bot.go where tool_use blocks are rendered)
- Gate the send on this config option
- Use the same global+per-agent override pattern as other config options (e.g. startup_notification)

## Update docs/CONFIG.md, SPEC.md. Write tests, commit with descriptive message, push.
