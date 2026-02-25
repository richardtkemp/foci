# Task: Fix Multiball "Idle" Message on Restart (#85)

## Bug
When clod restarts, multiball/pool bots (e.g. clodbot, clutchling) send "This bot is idle" messages. They should stay completely silent when idle.

## Likely causes to investigate
1. **Stale Telegram updates:** On restart, the bot processes queued getUpdates from while it was down. If someone (or something) sent a message during downtime, the idle bot responds.
2. **Startup notification leaking to secondary bots:** The startup notification system might be firing for multiball bots even though they have no active session.
3. **Session state:** On restart, multiball bots might not have their session mappings restored yet (even though multiball persist was implemented), causing them to treat incoming messages as "no session = idle".

## Investigation steps
1. Check what triggers the "idle" message — search for the string in the codebase
2. Trace the code path: bot receives update → checks if it has an active session → responds
3. Check if multiball persist (state store) is loaded before the bots start polling
4. Check getUpdates offset handling on startup — does clod skip stale updates?

## Fix
Depends on root cause. Likely one of:
- Set getUpdates offset to skip pre-restart messages
- Don't respond "idle" at all — just silently ignore messages to unassigned multiball bots
- Ensure state restoration happens before polling starts

## Verification
- Restart clod, verify no "idle" messages from multiball bots
- Multiball bots still respond correctly when assigned to a session
- `go build && go test ./... && go vet ./...`
