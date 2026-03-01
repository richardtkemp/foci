# Task: Per-agent keepalive, background, and show_tool_calls config

## Problem

`keepalive`, `background`, and `show_tool_calls` (in defaults) are flagged as unknown config keys because:
- `KeepaliveConfig` and `BackgroundConfig` only exist on the top-level `Config` struct
- `ShowToolCalls` isn't on `DefaultsConfig`

But the user wants `[agents.keepalive]` and `[agents.background]` to work per-agent, and `show_tool_calls` to work in `[defaults]`.

## Design principle

All config should be available per-agent as well as globally, wherever possible and reasonable. Per-agent overrides global. This is the same pattern already used for `show_tool_calls` on `AgentConfig` (pointer, nil = use global), `effort`, `thinking`, `max_tool_loops`, etc.

## Changes needed

1. **Add `Keepalive KeepaliveConfig` and `Background BackgroundConfig` to `AgentConfig`** with `toml:"keepalive"` and `toml:"background"`. Keep the existing top-level fields as globals.

2. **Add `ShowToolCalls` to `DefaultsConfig`** with appropriate type so `show_tool_calls = false` works in `[defaults]`. This becomes the global default that per-agent `show_tool_calls` overrides.

3. **Resolution logic:** When code needs keepalive/background config for an agent, check the agent's config first, fall back to global. Same pattern as existing per-agent overrides. Add a helper if needed.

4. **Make sure the existing consumers of top-level `cfg.Keepalive` and `cfg.Background`** (in main.go and wherever else) resolve per-agent properly — each agent should use its own keepalive/background config if set, otherwise the global.

5. **Update docs/CONFIG.md** with the new per-agent options.

Commit separately from the other batch. Push when done.
