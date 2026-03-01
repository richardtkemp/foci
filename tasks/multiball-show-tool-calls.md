# Bug: Shared multiball bots don't inherit agent-specific display settings

## Problem

When a shared multiball bot is acquired by an agent via `AcquireMultiball()`, only the agent and command registry are re-wired (`SetAgentAndCommands`). Display settings — `show_tool_calls`, `show_thinking`, `display_width` — remain at whatever the shared pool defaults were (global `telegram.*` values set at startup in main.go ~line 660).

Per-agent multiball bots (created in the agent loop ~line 2368) correctly check agent config first, falling back to global. But shared pool bots skip this entirely.

The same issue exists in the **restore path** (~line 2852) where saved multiball sessions are restored on startup — `SetAgentAndCommands` is called but display settings are not updated.

## Fix

In both the **fork path** (~line 2063) and **restore path** (~line 2852), after `SetAgentAndCommands`, apply agent-specific display settings. The pattern already exists in the per-agent multiball creation code (~line 2368-2382):

```go
if acfg.ShowToolCalls != nil {
    mbBot.SetShowToolCalls(string(*acfg.ShowToolCalls))
} else {
    mbBot.SetShowToolCalls(string(p.cfg.Telegram.ShowToolCalls))
}
if acfg.ShowThinking != nil {
    mbBot.SetShowThinking(string(*acfg.ShowThinking))
} else {
    mbBot.SetShowThinking(string(p.cfg.Telegram.ShowThinking))
}
if acfg.DisplayWidth != nil {
    mbBot.SetDisplayWidth(*acfg.DisplayWidth)
} else {
    mbBot.SetDisplayWidth(p.cfg.Telegram.DisplayWidth)
}
```

Extract this into a helper (e.g. `applyAgentDisplaySettings(bot, acfg, globalCfg)`) and call it:
1. In the per-agent multiball creation loop (replace the inline code)
2. After `SetAgentAndCommands` in the fork path
3. After `SetAgentAndCommands` in the restore path

## Files to change

- `main.go` — extract helper, apply in fork + restore paths

## TODO

#215
