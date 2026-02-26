# Task: Investigate Anthropic OAuth Token Lifecycle

## Goal
Thorough investigation of how Anthropic OAuth tokens work in this codebase — where they come from, where they're used, and how they're refreshed.

## Context
- This is a Go application at `/home/rich/git/clod`
- It uses Anthropic's API via OAuth (Claude Max subscription)
- There's an OAuth auto-refresh system that was recently built but the proactive refresh never fired (TODO #125)
- The OAuth flow uses a relay server for headless auth

## Questions to Answer

1. **Where do tokens come from?**
   - How is the initial OAuth flow triggered?
   - What relay/callback mechanism is used?
   - Where are tokens stored on disk?
   - What fields are stored (access_token, refresh_token, expires_at, scopes)?

2. **Where are tokens used?**
   - Which API calls use the OAuth token?
   - How is the token injected into requests (header, etc.)?
   - Is there a central Token() getter or are tokens accessed directly?

3. **How are tokens refreshed?**
   - What's the proactive refresh mechanism? (background ticker, threshold)
   - What's the reactive refresh mechanism? (401 → refresh → retry)
   - What endpoint is used for refresh?
   - What's the token lifetime / expiry window?

4. **Why didn't refresh fire?**
   - The token expired at 17:58 on 2026-02-26 with no refresh attempt
   - Check if the background ticker is actually running
   - Check if the expiry time is being parsed correctly
   - Check logs for any refresh-related messages

## Deliverable
Write findings to `/home/rich/git/clod/tasks/anthropic-token-findings.md` with:
- Code references (file:line) for each component
- Data flow diagram (text-based)
- Any bugs or issues found
- Recommendations for fixing TODO #125

## Important
- Read the code, don't guess. Trace the actual flow.
- Check `oauth/` directory, any `token` or `auth` related files
- Check config for OAuth-related settings
- Look at git log for recent OAuth-related commits
