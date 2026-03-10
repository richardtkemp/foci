# Phase 2: Telegram.Bot Uses MessageHandler Interface

## Goal

Change `telegram.Bot` to hold `platform.MessageHandler` interface instead of concrete `*agent.Agent` type. This breaks the circular dependency, allowing `agent` package to import `telegram` in future phases.

## Current State
```go
// internal/telegram/bot.go
type Bot struct {
    agent *agent.Agent  // <-- concrete type
    // ...
}
func NewBot(token string, allowedUsers []string, ag *agent.Agent, ...) (*Bot, error)
```
## Target State
```go
// internal/telegram/bot.go
type Bot struct {
    handler platform.MessageHandler  // <-- interface type
    // ...
}
func NewBot(token string, allowedUsers []string, handler platform.MessageHandler, ...) (*Bot, error)
```
## Problem: Attachment Type Mismatch
The `MessageHandler` interface uses `platform.Attachment`, but `agent.Agent` uses `agent.Attachment`. These have different field names:
```go
// platform.Attachment (in internal/platform/types.go)
type Attachment struct {
    Type      string  // <-- "Type"
    Data      []byte
    MimeType  string
    SavedPath string
}
// agent.Attachment (in internal/agent/agent.go)
type Attachment struct {
    MediaType string  // <-- "MediaType" (different!)
    Data      []byte
    SavedPath string
}
```
## Solution
Rename `agent.Attachment.MediaType` to `MimeType` to match `platform.Attachment`. The fields become:
- `MimeType` (was `MediaType`)
- `Data`
- `SavedPath`

**Note:** The `Type` field in `platform.Attachment` is not removed in this phase to avoid breaking changes in other packages. It's left as-is (empty string during conversion). It may have future uses for other platform implementations.

 Leave it as-is (empty string during conversion).
## Detailed Changes
### 1. Rename agent.Attachment field (internal/agent/agent.go)
**File:** `internal/agent/agent.go`
```go
// BEFORE
type Attachment struct {
    MediaType string // "image/jpeg", "image/png", "application/pdf", etc.
    Data      []byte
    SavedPath string
}
// AFTER
type Attachment struct {
    MimeType  string // "image/jpeg", "image/png", "application/pdf", etc.
    Data      []byte
    SavedPath string
}
```
**Note:** The `Type` field in `platform.Attachment` is not removed in this phase. It's left as-is (empty string during conversion). It may have future uses for other platform implementations. Leave it as-is (empty string during conversion).
### 2. Update agent.Attachment usages (internal/agent/turn_message.go)
**File:** `internal/agent/turn_message.go`
```go
// Line 39: BEFORE
if img.MediaType == "application/pdf" {
// Line 39: AFTER
if img.MimeType == "application/pdf" {
// Line 59: BEFORE
data, mediaType := img.Data, img.MediaType
// Line 59: AFTER
data, mediaType := img.Data, img.MimeType
```
### 3. Update agent.Attachment usages in tests (internal/agent/agent_attachments_test.go)
Run this command:
```bash
sed -i 's/MediaType/MimeType/g' internal/agent/agent_attachments_test.go
```
Or manually change all occurrences of `MediaType` to `MimeType` in this file.
### 4. Change Bot struct field (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`
```go
// Line 71: BEFORE
agent *agent.Agent
// Line 71: AFTER
handler platform.MessageHandler
```
### 5. Change NewBot constructor signature (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`
```go
// Line 140: BEFORE
func NewBot(token string, allowedUsers []string, ag *agent.Agent, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) (*Bot, error)
// Line 140: AFTER
func NewBot(token string, allowedUsers []string, handler platform.MessageHandler, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string) (*Bot, error)
```
### 6. Update struct literal in NewBot (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`
```go
// Line 167-168: BEFORE
agent: ag,
// Line 167-168: AFTER
handler: handler,
```
### 7. Rename SetAgentAndCommands method (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`
```go
// Line 360: BEFORE
func (b *Bot) SetAgentAndCommands(ag *agent.Agent, cmds *command.Registry) {
    b.agent = ag
    b.commands = cmds
}
// Line 360: AFTER
func (b *Bot) SetHandlerAndCommands(handler platform.MessageHandler, cmds *command.Registry) {
    b.handler = handler
    b.commands = cmds
}
```
### 8. Update b.agent.Warnings() nil check (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`  
**Function:** `receiveMessage` (around line 775)
**Search for:**  
```go
if b.agent == nil || b.agent.Warnings() == nil {
```
```go
// Line 778: BEFORE
if b.agent == nil || b.agent.Warnings() == nil {
// Line 778: AFTER
if b.handler == nil || b.handler.Warnings() == nil {
```
### 9. Update b.agent.TransformMessage call (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`  
**Function:** `receiveMessage` (around line 904)
**Search for:**  
```go
if b.agent != nil {
    if transformed := b.agent.TransformMessage(text); transformed != text {
        // ...
}
```
```go
// Lines 905-906: BEFORE
if b.agent != nil {
    if transformed := b.agent.TransformMessage(text); transformed != text {
        // ...
}
// Lines 905-906: AFTER
if b.handler != nil {
    if transformed := b.handler.TransformMessage(text); transformed != text {
        // ...
}
```
### 10. Update b.agent.HandleMessage calls (internal/telegram/bot.go)
**File:** `internal/telegram/bot.go`  
**Function:** `processAgentMessage` (around line 1084)
**Search for:**  
```go
if len(qm.images) > 0 {
    agentImages := make([]agent.Attachment, len(qm.images))
    for i, img := range qm.images {
        agentImages[i] = agent.Attachment{MediaType: img.mediaType, Data: img.data, SavedPath: img.savedPath}
    }
    response, err = b.agent.HandleMessageWithAttachments(turnCtx, sk, qm.text, agentImages)
} else {
    response, err = b.agent.HandleMessage(turnCtx, sk, qm.text)
}
```
```go
// Lines 1189-1197: BEFORE
if len(qm.images) > 0 {
    agentImages := make([]agent.Attachment, len(qm.images))
    for i, img := range qm.images {
        agentImages[i] = agent.Attachment{MediaType: img.mediaType, Data: img.data, SavedPath: img.savedPath}
    }
    response, err = b.agent.HandleMessageWithAttachments(turnCtx, sk, qm.text, agentImages)
} else {
    response, err = b.agent.HandleMessage(turnCtx, sk, qm.text)
}
// Lines 1189-1197: AFTER
if len(qm.images) > 0 {
    platformImages := make([]platform.Attachment, len(qm.images))
    for i, img := range qm.images {
        platformImages[i] = platform.Attachment{MimeType: img.mediaType, Data: img.data, SavedPath: img.savedPath}
    }
    response, err = b.handler.HandleMessageWithAttachments(turnCtx, sk, qm.text, platformImages)
} else {
    response, err = b.handler.HandleMessage(turnCtx, sk, qm.text)
}
```
**Important notes:**
1. The variable name changes from `agentImages` to `platformImages`
2. The type name changes from `agent.Attachment` to `platform.Attachment`
3. The handler references change from `b.agent` to `b.handler`
4. The agent package import remains for `agent.TurnCallbacks`, `agent.WithTurnCallbacks`, and `agent.WithTrigger` - these are context utilities, not the Agent type itself
### 11. Update bot_media.go nil checks (internal/telegram/bot_media.go)
**File:** `internal/telegram/bot_media.go`
```go
// Line 372: BEFORE
if b.agent == nil || b.agent.Warnings() == nil {
// Line 372: AFTER
if b.handler == nil || b.handler.Warnings() == nil {
// Line 399: BEFORE
if b.agent == nil || b.agent.Warnings() == nil {
// Line 399: AFTER
if b.handler == nil || b.handler.Warnings() == nil {
```
### 12. Update callers in cmd/foci-gw/agents.go
**File:** `cmd/foci-gw/agents.go`

**Function:** `setupTelegram` (around line 574)

**Find:** `NewBot` call (around line 580)

```go
primaryBot, err := telegram.NewBot(telegramToken, allowedUsers, ag, cmds, lastMsgStore, acfg.ID)
```
**No code change needed** - Go automatically converts `*agent.Agent` to `platform.MessageHandler` since the agent implements the interface.

### 13. Update callers of SetAgentAndCommands
**File:** `cmd/foci-gw/post_agent_setup.go`

Search for: `SetAgentAndCommands`
```go
// BEFORE
mbBot.SetAgentAndCommands(ag, cmds)
// AFTER
mbBot.SetHandlerAndCommands(ag, cmds)
```
---
## Verification
After all changes:
```bash
make build
make test
```
All tests must pass
---
## Expected Errors During Migration
If you see this error
```
*agent.Agent does not implement platform.MessageHandler (wrong type for method HandleMessageWithAttachments)
    have HandleMessageWithAttachments(context.Context, string, string, []agent.Attachment) (string, error)
    want HandleMessageWithAttachments(context.Context, string, string, []platform.Attachment) (string, error)
```
It means step 1-3 (renaming MediaType to MimeType) wasn't completed.
---
## Success Criteria
- [ ] Bot struct uses `handler platform.MessageHandler` field
- [ ] NewBot takes `platform.MessageHandler` parameter
- [ ] All `b.agent` references changed to `b.handler`
- [ ] agent.Attachment.MediaType renamed to MimeType
- [ ] `make build` succeeds
- [ ] `make test` passes
- [ ] No import of `foci/internal/agent` in `internal/telegram/bot.go` (except for context functions)
---
## After Phase 2
The circular dependency is broken
- `telegram` imports `platform` (interface)
- `telegram` no longer imports `agent` (concrete type)
- `agent` can now import `telegram` (in future phases)
```
This enables Phase 3 where agents create their own platforms.
