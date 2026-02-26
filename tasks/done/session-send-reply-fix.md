# Task: Fix send_to_session reply_to=session behaviour

## Reported Issue
`reply_to=session` for `send_to_session` does not seem to work — replies go back to the caller.

## Current Behaviour Analysis (please verify this is correct)

When `reply_to=session`:
1. Tool skips `Append()` (your recent fix, good)
2. `sessionNotifyFn` is called, which runs `HandleMessage()` on the target session in a goroutine
3. `HandleMessage` appends the message, generates a response, returns the response text
4. `sessionNotifyFn` sends the response to the target's Telegram chat via `bot.SendText(resp)`
5. Meanwhile, the tool returns `"Message sent to session X (reply_to=session)."` to the calling agent

**Potential issues to investigate:**
- Does `HandleMessage` return its response text AND also send it via Telegram internally? That would mean the response gets sent twice — once by `HandleMessage`'s normal flow, and once by `sessionNotifyFn`.
- Is the calling agent somehow receiving the target's response? Check if there's any mechanism by which the target's response leaks back to the caller's session.
- The `notifier.Notify` path (reply_to=caller) — does the Notify callback also call HandleMessage? If so, the same double-processing could happen there too.

## Desired Behaviour
`reply_to=session` should be fire-and-forget from the caller's perspective:
- Inject the message into the target session
- Trigger the target session to process it (generate a response and deliver it to the target's own Telegram chat)  
- The caller just gets confirmation: "Message sent"
- Nothing from the target's response should route back to the caller

## Your Job
1. Read the code paths carefully — trace what happens for both `reply_to=caller` and `reply_to=session` end-to-end
2. Verify my analysis above is correct or identify where it's wrong
3. Fix the issue so `reply_to=session` works as described in "Desired Behaviour"
4. Make sure `reply_to=caller` still works correctly (response routes back to calling session)
5. Tests, commit and push
