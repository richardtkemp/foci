# Task: send_to_session Tool + Default Fork Prompt

## Three Changes

### 1. `send_to_session` tool

A new tool that lets an agent inject a message into another session.

**Parameters:**
- `session_key` (string, required) — target session key
- `message` (string, required) — message to send

**Behaviour:**
- Injects a user-role message into the target session
- The injected message MUST clearly identify its origin, e.g.: `[Message from session agent:clutch:multiball:mb-123456]\n\n<message>`
- The target session's agent will see and respond to it on its next turn
- Returns confirmation to the caller

**Implementation:** This is essentially `sessions.Append(targetKey, anthropic.Message{Role: "user", Content: ...})` plus triggering the target session to process it (same mechanism as AsyncNotifier / wake injection).

### 2. Default fork prompt

Currently, if `fork_prompt` is not configured, no prompt is injected and the forked agent has no idea it's a branch. Fix this:

- If `fork_prompt` is configured, read and use that file (existing behaviour)
- If `fork_prompt` is NOT configured, use a sensible built-in default

**Default fork prompt text:**
```
You are a branch session forked from the main session. You can communicate with other sessions using the send_to_session tool — provide the session key and your message.
```

Keep it minimal. Don't tell the agent to type /done — it doesn't need instructions on session lifecycle.

### 3. Drop the fake "Understood."

Currently the fork prompt is injected as a pair:
```
{role: "user", content: "<fork prompt>"}
{role: "assistant", content: "Understood."}
```

Remove the fake assistant response. Just inject the user message. The agent will respond to it naturally on its first turn in the branch session.

**Important:** The fork prompt is injected into the session history as a `role: "user"` message — it goes TO the agent, not from the agent. This is correct and should stay this way.

## Files Likely Involved

- `tools/` — new `send_to_session` tool (new file or added to existing)
- `main.go` — wire the tool, update fork prompt injection (drop "Understood.", add default)
- `config/config.go` — no changes needed (fork_prompt stays optional)
- Tests for the new tool
- SPEC.md, docs/CONFIG.md, docs/WIRING.md

## When done: write/update tests, update SPEC.md, docs/CONFIG.md, docs/WIRING.md, commit with descriptive message, push.
