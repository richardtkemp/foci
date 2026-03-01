# TODO #151: Add visibility note to environment block

## Problem
The environment block tells agents "The human only sees the conversation" but doesn't specify whether tool calls and thinking blocks are visible. This affects communication strategy — if users can see tool calls, agents don't need to narrate actions. If they can't, agents need to explain more.

## Current state
- `buildEnvironmentBlock()` in `main.go:2448` builds the environment text
- Line 2497 says: "The human only sees the conversation — they cannot see your system prompt, character files, or this environment block."
- Agent config has `show_tool_calls` (off/preview/full) and `show_thinking` (off/compact/true)
- These are resolved per-agent: agent override > defaults > telegram global

## Fix
After the existing "human only sees the conversation" line, add a line describing tool call and thinking visibility based on the resolved agent config values.

The resolved values are available via `acfg.ShowToolCalls` and `acfg.ShowThinking` (both pointers, may be nil — fall back to `cfg.Defaults` then `cfg.Telegram` globals).

Add something like:
```
Tool calls: {off|preview|full}. Thinking: {off|compact|shown}.
```

Where:
- **off**: "Tool calls are hidden from the user — narrate important actions in your replies."
- **preview**: "Tool calls are shown as brief previews (tool name only) — the user sees what tools you use but not the details."  
- **full**: "Tool calls are fully visible — the user can see your tool inputs and outputs."
- **thinking off**: "Your thinking is hidden."
- **thinking compact**: "Your thinking is available behind a toggle button."
- **thinking true**: "Your thinking is shown inline before each response."

## Resolution logic
Need to resolve the effective value the same way telegram/bot.go does. Check:
1. `acfg.ShowToolCalls` (agent-level override)
2. `cfg.Defaults.ShowToolCalls` (defaults section)
3. `cfg.Telegram.ShowToolCalls` (global telegram config)

Same for ShowThinking.

## Files to change
- `main.go` — `buildEnvironmentBlock()` function

## Tests
- Update `agent/environment_test.go` to verify visibility lines appear in output
