# Task: Config Command Fixes

Three changes to the `/config` command:

## 1. `/config available` — wrap in code block
The table output loses alignment in Telegram. Wrap the entire output in a markdown code block (triple backticks) to preserve spacing.

## 2. `/config` (no args) — show usage
Currently shows the full config dump. Change it to just show usage text:
```
/config toml — raw TOML of running config (secrets redacted)
/config table — formatted table of current config values
/config available — unset options with defaults
```

## 3. `/config table` — new mode
Move the current default behaviour (formatted readable config) to `/config table`. Also wrap this in a code block for spacing preservation, same as available.

## Summary of new behaviour
- `/config` → usage help
- `/config toml` → raw TOML (unchanged)
- `/config table` → current config as formatted table (was the old default, now in code block)
- `/config available` → unset options (unchanged, but wrapped in code block)

## Verification
- `/config` shows usage
- `/config table` shows formatted config in code block
- `/config available` shows unset options in code block
- `/config toml` unchanged
- Update tests, SPEC.md
- `go build && go test ./... && go vet ./...`
