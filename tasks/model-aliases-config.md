# Task: Move model aliases from hardcoded switch to config

## Problem
`resolveModel()` in `command/agents_new.go:192` has hardcoded model aliases:
```go
case "opus": return "claude-opus-4-6"
case "sonnet": return "claude-sonnet-4-6"
case "haiku": return "claude-haiku-4-5"
```

These go stale when Anthropic releases new versions. Should be config-driven.

## Solution

Add a `[models]` section to config:

```toml
[models]
aliases = { opus = "claude-opus-4-6", sonnet = "claude-sonnet-4-6", haiku = "claude-haiku-4-5" }
```

Or expanded:
```toml
[models.aliases]
opus = "claude-opus-4-6"
sonnet = "claude-sonnet-4-6"
haiku = "claude-haiku-4-5"
```

## Changes needed

1. **config/config.go** — add `Models` struct with `Aliases map[string]string` field, parse from TOML
2. **command/agents_new.go** — `resolveModel()` should look up aliases from config instead of hardcoded switch. Fall through to returning input as-is if no alias found.
3. **command/builtins.go** — the `/model` command at line 579 calls `resolveModel(args)` — needs access to config aliases
4. **The function signature may need to change** — currently `resolveModel(input string) string`, may need to accept aliases map or be a method on something that has config access
5. **Keep the hardcoded defaults as fallback** — if no `[models.aliases]` in config, use sensible defaults so it works out of the box
6. **Update docs/CONFIG.md** with the new section
7. **Update tests** in `command/agents_new_test.go`

## Important
- Keep it simple — just a string→string map lookup
- Empty alias value or missing section = use defaults
- The `/model` command and agent wizard both use `resolveModel()` — both need to work
- Commit and push when done
