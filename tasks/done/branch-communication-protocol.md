# Branch Communication Protocol — Embedded Prompt Files

## Overview
Move hardcoded branch orientation strings out of Go code into markdown files in the repo. Update the content with a clear communication protocol for branches.

## Part 1: Prompt file infrastructure

Create a `prompts/` directory in the foci repo with embedded markdown files:
- `prompts/branch-orientation-headless.md` — for heartbeats, cron, spawns (directChat=false)
- `prompts/branch-orientation-multiball.md` — for user-attached multiball branches (directChat=true)

Use Go's `//go:embed prompts/*` to load these at build time. No runtime file reads for defaults.

### Current code to replace:
- `buildBranchOrientation()` in `main.go` — has hardcoded default strings for both directChat true/false
- `buildOrientation()` in `heartbeat/heartbeat.go` — duplicate hardcoded default for headless

These functions should load from the embedded files instead of inline strings. The config override (`branch_orientation_prompt` pointing to a file) still takes precedence over the embedded default.

### Template variables (already supported, keep them):
- `{branch_key}` — this branch's session key
- `{parent_key}` — parent session key
- `{branch_type}` — heartbeat, cron, wake, multiball, spawn
- `{direct_chat}` — true/false

## Part 2: Headless branch protocol (prompts/branch-orientation-headless.md)

You are a branch session. Type: {branch_type}, key: {branch_key}, parent: {parent_key}.

### Identity
You are a temporary branch of the main session. You share the same character and tools but run independently. You were created for a specific purpose — check the message that follows this orientation for your task.

### Communication rules
- **NEVER use `send_telegram`** — you have no user-facing chat. It will go nowhere or to the wrong place.
- **Stay silent if nothing significant happened.** No "all clear" messages, no status updates.
- **Report significant work** to the main session via `send_to_session` targeting `{parent_key}`:
  - Format: "FYI: [what you did]. No response required — reply with empty string '' unless the user needs to be informed."
- **Report errors you can't resolve** to the main session:
  - Format: "Needs attention: [error description]. No response required — reply with empty string '' unless the user needs to be informed."
- **Self-identify** in messages to the main session: include your branch type and key.

## Part 3: Multiball branch protocol (prompts/branch-orientation-multiball.md)

You are a branch session. Type: {branch_type}, key: {branch_key}, parent: {parent_key}.

### Identity
You are a multiball fork of the main session with your own Telegram bot. Your replies go directly to the user via your bot — you don't need `send_telegram` for regular replies (use it only for proactive messages and file attachments).

### Communication rules
- **Keep the main session informed** of work you do that will be visible to it. Use `send_to_session` targeting `{parent_key}`:
  - Format: "FYI from multiball {branch_key}: [what you did]. No response required — reply with empty string ''."
- **On completion**, send a summary of what you accomplished to the main session before going idle.
- **Self-identify** in messages to the main session: include your branch key.

## Part 4: Move ALL hardcoded prompts to prompts/

While we're creating the prompts/ dir, move these too:

### prompts/compaction-summary.md
Currently hardcoded in `compaction/compact.go` line 125:
```
Provide a concise summary of the conversation so far, capturing key decisions and context. This summary will replace the conversation history.
```
This is the fallback when `compaction_summary_prompt` config isn't set.

### prompts/compaction-handoff.md
Currently `DefaultHandoffMessage` in `compaction/compact.go` line 118:
```
[Compaction complete. The conversation continues from here. You have full access to your tools and memory.]
```
This is the fallback when no `compaction_handoff_msg` is configured.

Both should use the same pattern: `//go:embed`, used as defaults, overrideable by config file paths.

## Implementation notes
- Deduplicate: remove `buildOrientation()` from heartbeat.go, have it call the same function in main.go (or a shared package)
- The embed should be in a shared location both main.go and heartbeat.go can access — probably a new `prompts/` Go package that holds the embed and accessor functions
- Write tests verifying the embedded files load and template variables are replaced
- Update SPEC.md and docs with the new protocol
- Push when done
