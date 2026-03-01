# Task: Change platform defaults for thinking and effort (#193)

## What
Change the default thinking mode from "off" to "adaptive" and default effort from "" (omit) to "low".

## Where
`config/config.go` — in the `Load()` function, after the BraindeadThreshold default block (~line 757), add:

```go
if cfg.Defaults.Thinking == "" && !md.IsDefined("defaults", "thinking") {
    cfg.Defaults.Thinking = "adaptive"
}
if cfg.Defaults.Effort == "" && !md.IsDefined("defaults", "effort") {
    cfg.Defaults.Effort = "low"
}
```

## Also update
1. Comments on `DefaultsConfig.Thinking` and `DefaultsConfig.Effort` fields (~line 350-351) — update "(default)" annotations
2. Comments on `AgentConfig.Thinking` (~line 114) — update default note  
3. `foci.toml.example` — add `thinking = "adaptive"` and `effort = "low"` to `[defaults]` section with comments
4. `docs/CONFIG.md` — update defaults table if thinking/effort defaults are documented

## Verification
- `go build ./...` and `go test ./...` and `go vet ./...`
- Verify: a new agent with no thinking/effort config should get thinking=adaptive, effort=low
- Verify: an agent explicitly setting thinking="" or effort="" should keep those values (md.IsDefined check)
