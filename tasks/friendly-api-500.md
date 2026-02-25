# Fix: Friendly error messages for API 500 errors

## Problem
When Anthropic returns a 500 error (e.g. mana exhaustion), the raw JSON error is shown to the user:
```
Error: send message: API error (status 500): {"type":"error","error":{"type":"api_error","message":"Internal server error"},"request_id":"req_..."}
```

This is ugly and unhelpful.

## Requirements
1. Catch API 500 errors and show a friendly message instead of raw JSON
2. Check ALL error paths — not just the main send path:
   - `[async_notify]` errors
   - `[telegram] agent error` path  
   - `[http] async send error` path
3. The friendly message should say something like "Anthropic API is temporarily unavailable (likely rate limited). Try again in a few minutes."
4. Log the full error at DEBUG level for diagnostics

## Context
We may already have friendly handling for 529/rate-limit errors. Extend that pattern to cover 500s too. Check how 529 errors are currently handled and apply the same approach.

## Docs
No doc changes needed.
