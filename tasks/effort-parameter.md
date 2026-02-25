# Task: Effort parameter — per-agent effort levels

## What
Add support for Anthropic's `effort` parameter (part of `output_config`). Controls how much work Claude does per turn — lower effort = shorter responses, fewer tool calls, less thinking.

## API change
Add to MessageRequest:
```go
type OutputConfig struct {
    Effort string `json:"effort,omitempty"` // "low", "medium", "high", "max"
}
```
Add `OutputConfig *OutputConfig` field to `MessageRequest`.

## Config
```toml
[defaults]
effort = "high"    # global default

[[agents]]
id = "clutch"
effort = "high"
```
Per-agent override, falling back to defaults, falling back to nothing (omit from request).

## Slash command
`/effort` — show current effort level
`/effort low|medium|high|max` — change effort for current session (runtime, not persisted to config)

This needs in-memory per-session state that overrides the config value. The agent struct should have a mutable effort field that the slash command can set, and the API request builder reads from.

## Implementation
1. Add `OutputConfig` to `anthropic/types.go` and wire into `MessageRequest`
2. Add `effort` to `AgentConfig` and `DefaultsConfig` in `config/config.go`
3. Add `effort` field to agent struct (mutable, set from config on init, overridable by command)
4. Wire effort into API request building
5. Add `/effort` slash command
6. Update `/config table` and `/config available` to show effort
7. Update docs: CONFIG.md, SPEC.md, CLI.md if relevant

## Also while you're in the API types

### Strict tool schemas
Add `Strict bool` field to `ToolDef` in types.go. Set to `true` for all tools. This guarantees Claude sends valid JSON matching the schema.

### Beta header cleanup
In the API client, we send `"anthropic-beta": "prompt-caching-2024-07-31,oauth-2025-04-20"`. Prompt caching is GA now — remove `prompt-caching-2024-07-31` from the beta header. Add a comment: "prompt-caching beta removed — caching is GA as of 2024. See https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching"

## Update docs
- docs/CONFIG.md — effort in defaults and agent tables
- SPEC.md — effort parameter section
