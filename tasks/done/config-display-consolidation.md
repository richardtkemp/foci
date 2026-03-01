# Config Display Settings Consolidation (#199)

## Problem

Three display settings (show_tool_calls, show_thinking, display_width) exist in both `[telegram]` and `[defaults]` config sections, creating a confusing three-level resolution:

```
[telegram].show_tool_calls → [defaults].show_tool_calls → [[agents]].show_tool_calls
```

The `[telegram]` copies exist because these were added before `[defaults]` existed. Now that `[defaults]` is the standard pattern for agent-level defaults, the `[telegram]` copies are redundant.

## Proposed Fix

1. **Remove** ShowToolCalls, ShowThinking, DisplayWidth from TelegramConfig
2. **Move defaults** into DefaultsConfig (already there — just need to set defaults via `md.IsDefined("defaults", ...)` instead of `md.IsDefined("telegram", ...)`)
3. **Update main.go** wiring to resolve from agent (already incorporates defaults fallback from config.go) without falling back to telegram
4. **Migration**: if someone has `show_tool_calls = "full"` under `[telegram]`, it should still work — check `[telegram]` section for these keys and warn/migrate to `[defaults]`

## Files to modify

- `config/config.go` — remove from TelegramConfig, update defaults in Load()
- `main.go` — update wiring (setupAgentBot, multiball bot setup) to use only agent config (which already has defaults applied)
- `docs/CONFIG.md` — move settings from [telegram] table to [defaults] or [[agents]] table
- `SPEC.md` — update if referenced

## Precedence after fix

```
[defaults].show_tool_calls → [[agents]].show_tool_calls
```

Two levels, not three. Clean.

## Also consolidate tts_rate (#271)

Same pattern: remove `[defaults].tts_rate`. Keep `[voice].tts_rate` as global (voice-specific, belongs in [voice]) and `[[agents]].tts_rate` as per-agent override.
