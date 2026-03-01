# Inline Keyboard for Slash Commands (#144)

## Goal
When a user types `/cmd` bare (no arguments), show an inline keyboard with available options instead of help text. Full typed commands (`/cmd arg`) still work as before.

## Architecture

### New field on Command struct
Add an optional `KeyboardOptions` field to `command.Command`:

```go
type KeyboardOption struct {
    Label    string // Button text shown to user
    Data     string // Callback data (command + args to execute)
    Row      int    // Which row this button goes in (0-indexed)
}

type Command struct {
    // ... existing fields ...
    KeyboardOptions func(ctx context.Context) []KeyboardOption // nil = no keyboard
}
```

Using a function (not static slice) because some commands need dynamic options (e.g. /model could show available models, /sessions shows active sessions).

### Bot dispatch change
In the command dispatch path (telegram/bot.go), when:
1. A command is recognized
2. Args are empty  
3. The command has `KeyboardOptions != nil`

Then: call `KeyboardOptions(ctx)`, build an `InlineKeyboardMarkup`, send it as a message with a brief label (e.g. "Select model:"). Don't execute the command.

### Callback handler
Extend the existing `handleCallbackQuery` in bot.go. Currently it only handles tool call expansion (`tc:` prefix). Add a new prefix for command callbacks, e.g. `cmd:`:

- Callback data format: `cmd:/model opus` (prefix + full command string)
- On callback: parse the command, execute it via the registry, answer the callback query with the result
- Edit the original keyboard message to show the result (e.g. "Model switched to: opus") and remove the keyboard

### Commands to add keyboards to

| Command | Options | Notes |
|---------|---------|-------|
| `/model` | opus, sonnet, haiku (from model aliases) | Dynamic — read from config |
| `/thinking` | off, adaptive | Static |
| `/effort` | low, medium, high | Static |
| `/voice` | on, off | Static (toggle) |
| `/config` | toml, table, available | Static subcommands |
| `/sessions` | (list active sessions) | Dynamic |
| `/compact` | (just a confirm button) | Single button "Compact now" |
| `/reset` | (just a confirm button) | Single button "Reset session" — destructive, confirm is good UX |

Don't add keyboards to: `/status`, `/ping`, `/version`, `/help`, `/cost`, `/cache`, `/context`, `/log`, `/errors`, `/tools`, `/prompts`, `/mana` — these are info-only commands that work fine with no args.

`/tmux` is complex (subcommands with their own args) — skip for now, could be nested keyboards later.

### Callback data size limit
Telegram limits callback_data to 64 bytes. `cmd:/model claude-opus-4-6` = 28 bytes, fine. But watch for long model IDs or session keys. If any would exceed 64 bytes, use a lookup table (store in bot state, keyed by short hash).

## Implementation notes

- The existing `handleCallbackQuery` switch can be extended with a `strings.HasPrefix(data, "cmd:")` branch
- Keep the tool call expansion (`tc:` prefix) working — don't break it
- Callback queries MUST be answered (even with empty AnswerCallbackQuery) or Telegram shows a loading spinner forever
- After executing the command via callback, edit the original message to show result and remove keyboard (use `EditMessageText` + remove `reply_markup`)

## Tests
- Test that commands with KeyboardOptions return keyboards when args empty
- Test that callback data is correctly parsed and executed
- Test that commands still work with typed args (no keyboard shown)

## Docs
- Update SPEC.md with the keyboard feature
- No config changes needed
