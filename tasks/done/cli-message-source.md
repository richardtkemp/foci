# Task: CLI Message Source — Text vs File

## Problem
`foci send` and `foci branch` currently take message text as trailing args. Need explicit flags for text vs file, and the ability to send file contents as the message.

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
foci send "hello world"
foci send --message-text "hello world"
foci send -mt "hello world"

# Send file contents:
foci send --message-file /home/foci/shared/prompts/memory-formation.md
foci send -mf /home/foci/shared/prompts/memory-formation.md

# With other flags:
foci send -a clutch --if-active 4h -mf tasks/review.md
foci branch --oneshot -a scout -mf /home/foci/shared/prompts/daily-health-check.md
```

## Implementation
1. Parse `--message-text`/`-mt` and `--message-file`/`-mf` flags in `cmd/foci/main.go`
2. For `-mf`, read the file with `os.ReadFile` and use contents as the message
3. Trailing args without a flag = implicit `-mt` (backward compat)
4. Error if both specified, error if file not found
5. Update usage/help text
6. Update docs/CLI.md

## Environment variables

ALL CLI flags should also be settable via env vars. Resolution: explicit flag > env var > default.

Naming convention: `FOCI_` prefix + uppercase flag name with hyphens as underscores.

| Flag | Env var |
|------|---------|
| `-a` / `--agent` | `FOCI_AGENT` |
| `-s` / `--session` | `FOCI_SESSION` |
| `--if-active` | `FOCI_IF_ACTIVE` |
| `--message-text` / `-mt` | `FOCI_MESSAGE_TEXT` |
| `--message-file` / `-mf` | `FOCI_MESSAGE_FILE` |
| `--no-compact` | `FOCI_NO_COMPACT` (any non-empty = true) |
| `--no-reset-hook` | `FOCI_NO_RESET_HOOK` (any non-empty = true) |
| `--oneshot` | `FOCI_ONESHOT` (any non-empty = true) |

`FOCI_ADDR` already exists — keep it as-is.

This means crontab entries can set agent once at the top:
```
FOCI_AGENT=clutch
FOCI_IF_ACTIVE=4h
*/30 * * * * foci send -mf /home/foci/shared/prompts/memory-formation.md
```

## Verification
- `foci send "text"` still works (backward compat)
- `foci send -mt "text"` works
- `foci send -mf somefile.md` sends file contents
- `foci branch -mf somefile.md` sends file contents
- Error on both flags, error on missing file
- All combinations with other flags (-a, -s, --if-active, --oneshot, etc.)
- `go build && go test ./... && go vet ./...`
