# Task: Branch identity — inject orientation message in CreateBranch

## Problem
Three code paths create branch sessions (multiball, cron/wake, spawn clone_current) but only multiball injects an orientation message telling the agent it's a branch. Cron and spawn branches don't know they're branches, leading to bugs like spawn sessions sending Telegram messages directly to the user (duplicating the parent session's work).

## Requirements

### 1. Move orientation message into CreateBranch
- All branches should get an orientation message injected automatically when created
- Remove the existing fork prompt injection from the multiball code path (main.go ~line 1567-1578) — it now happens in CreateBranch
- The message should be injected as the first user-role message in the branch session

### 2. Orientation message content
The message should include:
- That this is a branch session (not the main session)
- The branch session key (so the agent knows its own identity)
- The parent session key (so it can send_to_session back)
- The branch type (multiball, cron, spawn) if available
- Instruction NOT to send Telegram messages directly — communicate results back to the parent session using send_to_session

Sensible default text, e.g.:
```
You are a branch session. Your session key: {branch_key}. Parent session: {parent_key}. 
Do not send messages to Telegram directly — report results back to the parent session using send_to_session with the parent session key.
For multiball sessions: you have your own Telegram bot and CAN communicate with the user directly.
```

The multiball case is different — multiball branches DO have their own Telegram bot and should talk to the user. The orientation should reflect this. Consider passing branch type to CreateBranch so the message can be tailored, or use a simple flag like `directChat bool`.

### 3. Configurable
- Global default: `[sessions] branch_orientation_prompt` (path to prompt file, empty = use built-in default)
- Per-agent override: `branch_orientation_prompt` on `[[agents]]`
- The prompt file can use template variables: `{branch_key}`, `{parent_key}`, `{branch_type}`

### 4. Update CreateBranch signature
CreateBranch needs to know the parent key and branch key (it already does), plus enough info to build the orientation. Consider:
```go
type BranchOptions struct {
    NoResetHook bool
    BranchType  string // "multiball", "cron", "spawn"
    DirectChat  bool   // true for multiball (has own Telegram bot)
}
```
Consolidate so ALL callers use CreateBranchWithOptions.

## Update docs
- SPEC.md — branch identity section
- docs/CONFIG.md — new config fields
- Update tests
