# Task: Persist tmux watches across process restarts

## Problem
Tmux watches (inactivity monitoring goroutines) are in-memory only. When foci restarts (deploy, crash, manual restart), all watches are lost. The user has to manually re-set them, which they won't remember to do.

## Requirements
1. When a watch is set, persist it to the state store (same pattern as owned tmux sessions)
2. On startup, restore all persisted watches — re-launch the monitoring goroutines
3. When a watch is unwatched or the tmux session is killed, remove from persistent state
4. Handle edge cases: tmux session no longer exists on restore (clean up and skip), session key still valid

## Implementation Notes
- The tmux tool already uses `stateStore` for owned sessions (`tmux:agentID` key)
- Watches could be stored similarly, e.g. `tmux-watches:agentID` 
- Each watch needs: tmux session name, window index, threshold seconds, agent session key (for notification delivery)
- On restore, the agent session key from the original watch should still be valid (it's the agent's main session key, not a transient one)
- ClearAll should also clear persisted watches

## Compaction safety
Watch notifications delivered mid-compaction may be lost — the agent is in the middle of an API call and can't receive injected messages. The watch delivery mechanism should ensure the notification actually arrives:
- Option A: Queue the notification and deliver after the current API call completes
- Option B: Retry delivery if the session is busy (compacting)
- Option C: Check session state before delivering; if busy, wait and retry
Whatever approach you choose, verify that a watch firing during compaction doesn't silently drop the notification.

## Update docs
- SPEC.md if relevant
- docs/CONFIG.md if any new config
