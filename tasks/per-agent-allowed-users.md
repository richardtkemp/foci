# Task: Per-Agent allowed_users (#61)

## Problem
`allowed_users` is currently global in `[telegram]`. Any allowed user can message any agent's bot. We need per-agent user restrictions.

## Implementation

1. Add `allowed_users` field to `AgentConfig` (string slice, same format as global)
2. Resolution: if agent has `allowed_users` set, use that. Otherwise fall back to global `[telegram] allowed_users`
3. The check happens in the Telegram bot message handler — when a message arrives, verify the sender is allowed for that specific agent
4. If a non-allowed user messages an agent's bot, silently ignore (same as current behaviour for globally non-allowed users)

## Config example
```toml
[telegram]
allowed_users = ["123", "456"]  # global default

[[agents]]
id = "clutch"
# no allowed_users — uses global ["123", "456"]

[[agents]]
id = "fotini"
allowed_users = ["456", "789"]  # only these users, NOT global
```

## Notes
- The bot struct needs to know its agent's allowed users, not just the global list
- Wire it the same way as other per-agent overrides (in main.go setupAgent or similar)
- Update docs: CONFIG.md (both tables), SPEC.md

## Verification
- Agent with `allowed_users` only accepts those users
- Agent without `allowed_users` falls back to global
- `go build && go test ./... && go vet ./...`
