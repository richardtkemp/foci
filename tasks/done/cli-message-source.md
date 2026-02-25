# Task: CLI Message Source — Text vs File

## Problem
`clod send` and `clod branch` currently take message text as trailing args. Need explicit flags for text vs file, and the ability to send file contents as the message.

## New flags

For both `send` and `branch`:
- `--message-text <text>` / `-mt <text>` — send this text as the message (current behaviour, just explicit)
- `--message-file <path>` / `-mf <path>` — read file at path and send its contents as the message

## Backward compatibility
- Trailing args (no flag) should still work as implicit `--message-text` for backward compat
- If both `--message-text` and `--message-file` are given, error
- If `--message-file` path doesn't exist, error with clear message

## Examples
```bash
# These are equivalent:
clod send "hello world"
clod send --message-text "hello world"
clod send -mt "hello world"

# Send file contents:
clod send --message-file /home/clod/shared/prompts/memory-formation.md
clod send -mf /home/clod/shared/prompts/memory-formation.md

# With other flags:
clod send -a clutch --if-active 4h -mf tasks/review.md
clod branch --oneshot -a scout -mf /home/clod/shared/prompts/daily-health-check.md
```

## Implementation
1. Parse `--message-text`/`-mt` and `--message-file`/`-mf` flags in `cmd/clod/main.go`
2. For `-mf`, read the file with `os.ReadFile` and use contents as the message
3. Trailing args without a flag = implicit `-mt` (backward compat)
4. Error if both specified, error if file not found
5. Update usage/help text
6. Update docs/CLI.md

## Environment variables

ALL CLI flags should also be settable via env vars. Resolution: explicit flag > env var > default.

Naming convention: `CLOD_` prefix + uppercase flag name with hyphens as underscores.

| Flag | Env var |
|------|---------|
| `-a` / `--agent` | `CLOD_AGENT` |
| `-s` / `--session` | `CLOD_SESSION` |
| `--if-active` | `CLOD_IF_ACTIVE` |
| `--message-text` / `-mt` | `CLOD_MESSAGE_TEXT` |
| `--message-file` / `-mf` | `CLOD_MESSAGE_FILE` |
| `--no-compact` | `CLOD_NO_COMPACT` (any non-empty = true) |
| `--no-reset-hook` | `CLOD_NO_RESET_HOOK` (any non-empty = true) |
| `--oneshot` | `CLOD_ONESHOT` (any non-empty = true) |

`CLOD_ADDR` already exists — keep it as-is.

This means crontab entries can set agent once at the top:
```
CLOD_AGENT=clutch
CLOD_IF_ACTIVE=4h
*/30 * * * * clod send -mf /home/clod/shared/prompts/memory-formation.md
```

## Verification
- `clod send "text"` still works (backward compat)
- `clod send -mt "text"` works
- `clod send -mf somefile.md` sends file contents
- `clod branch -mf somefile.md` sends file contents
- Error on both flags, error on missing file
- All combinations with other flags (-a, -s, --if-active, --oneshot, etc.)
- `go build && go test ./... && go vet ./...`
