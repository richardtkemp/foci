# Task: Don't flag stale sessions as cache busts

## Problem
Cache bust detection warns when cache_read drops to 0. But if a session has been idle for >1 hour, the Anthropic cache has naturally expired — this isn't a cache bust, it's just a cold start. The current warning is misleading:

```
⚠️ Cache bust: read dropped 15918 → 0 on agent:fotini:8792716180
```

## Fix
Suppress the cache bust warning if the time since the last API call on that session exceeds a threshold (e.g. 1 hour, or make it configurable). Anthropic's cache TTL is 5 minutes, so anything over ~10 minutes of inactivity should not be flagged.

## Implementation
1. Find where cache bust detection happens (likely in the API logging or response handling)
2. Track the timestamp of the last API call per session
3. When cache_read drops to 0, check: was the last call on this session more than N minutes ago?
4. If yes: suppress the warning (or log at DEBUG level instead of WARN)
5. If no: this is a genuine cache bust, warn as before

## Configuration
Either a hardcoded 10-minute threshold (Anthropic cache TTL is 5 min, 10 min gives margin) or a configurable `cache_bust_idle_threshold` in `[logging]`.

## Verification
- Stale sessions (>10min idle) don't trigger cache bust warnings
- Active sessions with genuine cache busts still warn
- `go build && go test ./... && go vet ./...`
