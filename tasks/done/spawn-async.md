# Task: Spawn Should Not Block Parent Session

## Problem

When an agent uses `spawn` (especially `inherit` mode — the headless self-fork), the parent session blocks until the spawned session completes. The parent agent can't do anything else while waiting. This defeats the purpose of spawn as delegation — it's currently just a slow synchronous function call.

## Current Behaviour

1. Parent agent calls `spawn` with a prompt
2. Parent blocks waiting for the spawned session to complete
3. Spawn finishes → result returned to parent
4. Parent can only now continue

This means:
- Agent can't respond to user messages while a spawn is running
- Agent can't manage multiple spawns in parallel (despite `max_concurrent_spawns` existing)
- No better than just doing the work inline

## Desired Behaviour

1. Parent agent calls `spawn` with a prompt
2. Spawn kicks off in the background immediately
3. Parent gets an immediate acknowledgment (spawn ID, or similar handle)
4. Parent is free to continue — respond to messages, do other work, launch more spawns
5. When spawn completes, result is delivered back to the parent asynchronously (same pattern as auto-backgrounded exec/http_request)

## Design Notes

- This should follow the same async delivery pattern as exec auto-background and the new http_request auto-background (commit 511bbbfa). The notifier/injection mechanism already exists.
- The `none` and `full` context modes (one-shot queries to other models) could arguably stay synchronous since they're fast. But `inherit` mode (full tool access, multi-step work) MUST be async. Consider making all modes async for consistency, or at minimum `inherit`.
- `max_concurrent_spawns` already exists in config — this should actually enforce it now that multiple spawns can genuinely run in parallel.
- The spawn result delivery should include the spawn ID/prompt so the parent knows which spawn finished.
- Consider whether spawn needs an explicit `background` parameter like exec/http_request, or whether it should always be async (since the whole point is delegation).

## Files Likely Involved

- Wherever spawn is implemented (check tools/ or agent/ for spawn handling)
- The notifier/async result delivery mechanism (same as exec/http_request use)
- main.go if wiring changes are needed
- SPEC.md, docs/ for documentation

## Observed Behaviour (testing from live agent)

When the parent agent calls `spawn` with `inherit` context:
- The parent is completely blocked until the spawn returns — cannot respond to user messages
- Tool calls from the spawned session appear in the **parent's** chat (user can see them)
- `last` API call metadata shows the parent's session key (`agent:clutch:chat:5970082313`), not a separate session
- This suggests inherit mode may be executing inline in the parent session rather than as a true separate session
- The spawn result text sometimes confabulates rather than actually executing tools (e.g. claimed to fetch a 10-second delay endpoint but completed in 3.7s without actually fetching)

**How spawn is supposed to work:** Spawn starts a completely separate session. The child session runs independently with its own tool calls, its own turns, its own context. The parent never sees the child's intermediate tool calls or reasoning. Only a single final text result is returned to the parent when the child session completes. Currently this is broken — the child appears to execute inline in the parent.

## When done: update SPEC.md, docs/CONFIG.md, docs/WIRING.md, write/update tests, commit with descriptive message, push.
