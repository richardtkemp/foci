# Fix: Handle API 500 errors with retry + friendly message

## Problem
When Anthropic returns 500 errors (server-side incidents), the raw JSON error is shown to the user and the request fails immediately. These are transient errors that should be retried.

## Requirements

### 1. Retry with exponential backoff
- On 500 errors, retry automatically with exponential backoff
- Suggested: 3 retries, starting at 2s, doubling each time (2s, 4s, 8s)
- Log each retry at WARN level: "API server error (attempt 2/3), retrying in 4s..."
- Only give up after all retries exhausted

### 2. Friendly error message on final failure
- After retries exhausted, show: "Anthropic API is temporarily unavailable (server error). Try again in a few minutes."
- NOT the raw JSON error
- Log the full error at DEBUG level for diagnostics

### 3. Check ALL error paths
The friendly message must work in all paths that surface errors to users:
- Main agent send path
- `[async_notify]` errors
- `[telegram] agent error` path
- `[wake] async error` path
- `[http] async send error` path

The retry logic should be in the anthropic client itself (SendMessage method), not in the agent layer — that way all callers benefit.

### 4. Distinguish from rate limits
- 429/529 = rate limit / overloaded → existing behaviour (mana callback, no retry)
- 500 = server error → retry with backoff, then friendly message

## Context
We already handle 429/529 with `IsRateLimit()`/`IsOverloaded()` in agent.go. The 500 handling should be in the client layer (retry) with a friendly fallback in the agent layer (message).

## Docs
No doc changes needed — this is error handling behaviour.
