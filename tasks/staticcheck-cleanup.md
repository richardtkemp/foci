# Task: Fix staticcheck and dupl findings

## Context
Codebase audit found 11 staticcheck issues and 4 production duplication spots. Fix all staticcheck findings and the 2 non-table-alignment duplications (table alignment is a separate task).

## Staticcheck findings — fix all

### Dead code (U1000) — remove
- `command/builtins.go:836` — field `cmds` is unused
- `config/display.go:836` — func `writeField` is unused

### Error string conventions (ST1005) — fix
Go convention: error strings should be lowercase, no trailing punctuation.
- `agent/agent.go:659` — capitalized + ends with punctuation
- `tools/files.go:154` — ends with punctuation
- `tools/syntax.go:53` — capitalized

### Unused test values (SA4006) — fix
- `command/agents_new_test.go:265,763` — `resp` assigned but unused
- `tools/memory_test.go:68,105` — `tool` assigned but unused
- `tools/tmux_test.go:1248` — `result` assigned but unused

## Duplication fixes

### telegram/bot.go — SendDocument/SendVoice duplication
Lines 1004-1059: `SendDocument` and `SendVoice` are near-identical. Lines 1086-1125: `SendDocumentToChat` and `SendVoiceToChat` are also near-identical. Refactor: extract a shared helper like `sendFile(chatID int64, filePath string, sendFunc)` and have all four call it.

### main.go — if_active/if_inactive gating
Lines 697-724: The `--if-active` and `--if-inactive` blocks are mirror images. Extract a helper function like `checkActivityGating(req, agentID) (skip bool, response string)` used by both /send and /wake handlers.

## Don't fix
- `command/builtins.go:298,1329` column width measurement — covered by separate table-alignment task
- Test file duplication — low priority, normal for Go tests

## Requirements
- Run `staticcheck ./...` after — should be clean
- Run `go test ./...` — all pass
- Commit and push when done
