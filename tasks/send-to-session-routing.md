# Task: send_to_session Reply Routing (#90)

## Problem
When agent A uses `send_to_session` to message agent B's session, B's reply always routes back to A (the calling session). This is correct for inter-agent coordination, but wrong when you want B to reply to its own user.

Example: Clutch tells Fotini "message Eleni about dinner" — Fotini's reply goes back to Clutch instead of to Eleni's chat.

## Solution
Add a `reply_to` parameter to the send_to_session tool:

```
reply_to: "caller" (default, current behaviour) | "session" (reply to target session's own chat)
```

When `reply_to = "session"`:
- The message is injected into B's session as usual
- B's response goes to B's own Telegram chat (the one associated with that session key)
- No response is sent back to A's session

When `reply_to = "caller"` (default):
- Current behaviour — B's response is delivered back to A

## Implementation
1. Add `reply_to` field to the send_to_session tool definition (enum: "caller", "session")
2. In the HTTP handler for /send (or wherever send_to_session is processed):
   - If reply_to=session: inject message, let the normal session response flow handle it (reply goes to the session's chat)
   - If reply_to=caller: current behaviour (return response to caller)
3. Update tool description so agents understand the option

## Tool definition update
```json
{
  "name": "send_to_session",
  "parameters": {
    "session_key": "target session key",
    "message": "message to send",
    "reply_to": "Where to send the reply: 'caller' (back to this session, default) or 'session' (to the target session's own chat)"
  }
}
```

## Verification
- send_to_session with reply_to=caller works as before
- send_to_session with reply_to=session delivers response to target's chat
- Default (no reply_to) = caller behaviour
- `go build && go test ./... && go vet ./...`
