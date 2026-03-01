# Task: Replace OAuth with `claude setup-token` Authentication

## Problem

Foci currently uses Anthropic's OAuth flow, reading credentials from `~/.claude/.credentials.json` (the `claudeAiOauth` field). This is fragile:

- Tokens expire and need constant refreshing
- Refresh only works if Claude Code is running and refreshing tokens for us
- When CC isn't running, foci gets 401s and dies
- We're parasitically depending on Claude Code's token lifecycle

## Solution

Replace the OAuth flow with `claude setup-token`, which provides a long-lived token that doesn't need refreshing.

## What to do

1. **Understand `claude setup-token`**: Run `~/.local/bin/claude setup-token --help` and investigate what kind of token it produces, where it stores it, and how long it lives. Check `~/.claude/.credentials.json` structure after setup-token vs after oauth login.

2. **Replace the OAuth client in foci**: The current OAuth implementation is in `anthropic/oauth.go`. Replace it with a simpler system that:
   - Reads the long-lived token from wherever `claude setup-token` stores it
   - Uses it directly for API auth (likely just a Bearer token)
   - No refresh logic needed (or minimal)

3. **Create an onboarding flow**: New users need guidance. When foci starts and finds no valid token:
   - Log a clear message explaining they need to run `claude setup-token`
   - Optionally: a `foci setup` CLI command that guides them through it
   - Don't just silently fail with 401s

4. **Clean up**: Remove the OAuth refresh ticker, the credential file locking, the `maybeRefresh` logic — all the complexity that exists only because OAuth tokens expire.

## Key files
- `anthropic/oauth.go` — current OAuth implementation
- `anthropic/client.go` — where auth is used for API calls
- `agent/agent.go` — startup/initialization
- `~/.claude/.credentials.json` — current credential storage

## Important
- Update SPEC.md and docs/CONFIG.md
- Write tests
- Don't break existing sessions — migration path for current oauth users
- Commit and push when done
