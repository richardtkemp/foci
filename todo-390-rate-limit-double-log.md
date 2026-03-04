# Fix: Rate-limited actions log twice (WARN + ERROR)

## Context

When a 429 rate limit occurs, two log lines appear for the same event:
```
WARN  [agent:clutch] rate limited: status=429 retry_after=5333
ERROR [keepalive] proactive warning turn error: rate limited — mana exhausted
```

**Root cause:** `classifyAPIError()` in `agent/agent.go:1248-1253` logs WARN *and* returns a wrapped error. Every caller of `HandleMessage()` then logs that returned error as ERROR — producing a duplicate.

The same pattern applies to overloaded (line 1256) and retryable server errors (line 1261) — they all WARN inside `classifyAPIError()` then the caller ERROR-logs the returned error.

## Fix

**Remove the WARN logs from `classifyAPIError()`** (lines 1249, 1256, 1261). The function's job is to *classify and return* — logging is the caller's responsibility. The callers already log the error with appropriate context (component tag, what they were doing).

Keep the `Debugf` for server error detail (line 1260) since that's extra diagnostic info not in the returned error.

### File: `agent/agent.go`

In `classifyAPIError()`:
- **Line 1249:** Remove `a.logger().Warnf("rate limited: status=%d retry_after=%s", ...)`
- **Line 1256:** Remove `a.logger().Warnf("overloaded: status=%d (retries exhausted)", ...)`
- **Line 1261:** Remove `a.logger().Warnf("API server error (status %d)", ...)`

Three deletions, nothing else changes. The `RateLimitFunc` callbacks and error return values stay as-is.

## Verification

```
go test ./agent/ -run TestClassify
go vet ./...
```

Then trigger a rate limit in practice — should see one ERROR log from the caller, not WARN+ERROR.
