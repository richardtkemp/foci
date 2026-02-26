# Anthropic OAuth Token Lifecycle — Investigation Findings

## Summary

The OAuth token system has proactive (background ticker) and reactive (401 retry) refresh mechanisms. The proactive refresh failed because of **two bugs in the Start() function** that can cause tokens to expire without any refresh attempt.

---

## 1. Where Do Tokens Come From?

### Initial OAuth Flow
- **No OAuth login flow in this codebase.** Tokens come from the official Claude Code CLI, which stores them in `~/.claude/.credentials.json`.
- The codebase only reads and refreshes existing tokens — it never initiates OAuth device code or authorization flows.

### Token Storage
- **File:** `~/.claude/.credentials.json` (configurable via `credentials_file` in config)
- **Code:** `anthropic/oauth.go:246` — `readCredentials()` reads with shared flock
- **Format:**
```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "sk-ant-ort01-...",
    "expiresAt": 1772156251882,
    "scopes": ["user:inference", "user:mcp_servers", ...],
    "subscriptionType": "max",
    "rateLimitTier": "default_claude_max_5x"
  }
}
```

### Fields Stored
| Field | Type | Description |
|-------|------|-------------|
| `accessToken` | string | Bearer token for API calls |
| `refreshToken` | string | Token for refresh requests |
| `expiresAt` | int64 | Unix milliseconds (fixed in commit c71bd301) |
| `scopes` | []string | OAuth scopes (fixed in commit 28870d20) |

---

## 2. Where Are Tokens Used?

### Central Token Getter
- `OAuthManager.Token()` at `anthropic/oauth.go:110`
- Thread-safe, returns current `accessToken`

### API Client Integration
- **Message API:** `anthropic/client.go:130`
  - `httpReq.Header.Set("Authorization", "Bearer "+c.getToken())`
- **Usage API:** `anthropic/usage.go:87`
  - Same pattern with `getToken()`

### Token Injection Flow
```
main.go:247 → NewClientWithTokenFunc(mgr.Token, timeout)
           → client.tokenFunc = mgr.Token
           → client.getToken() → mgr.Token() → accessToken
           → Header: "Authorization: Bearer <token>"
```

### UsageClient
- `main.go:473` — Uses `oauthMgr.Token` if OAuthManager available
- Falls back to reading credentials file directly or static token

---

## 3. How Are Tokens Refreshed?

### Proactive Refresh (Background)
- **Code:** `anthropic/oauth.go:140-163` — `Start()` method
- **Mechanism:** `time.Ticker` fires every `RefreshInterval = 5 minutes`
- **Trigger condition:** `remaining < RefreshWindow (30min) && remaining > 0`
- **Action:** Calls `refresh()` which POSTs to `OAuthRefreshURL`

### Reactive Refresh (401 Retry)
- **Code:** `anthropic/client.go:204-217`
- **Mechanism:** On 401 response, calls `refreshFunc(tokenBefore)` then retries once
- **Dedup:** `RefreshIfNeeded()` checks if token already changed or refresh in progress

### Refresh Endpoint
- **URL:** `https://console.anthropic.com/api/oauth/token`
- **Payload:**
```json
{
  "grant_type": "refresh_token",
  "refresh_token": "...",
  "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
}
```

### Token Lifetime
- Based on test data: `expires_in: 28800` seconds = **8 hours**
- Refresh writes updated token back to credentials file with exclusive flock

---

## 4. Why Didn't Refresh Fire?

### Root Cause: Two Bugs in Start()

**BUG 1: No immediate check at startup**
```go
// oauth.go:140-163
func (m *OAuthManager) Start() {
    go func() {
        defer close(m.done)
        ticker := time.NewTicker(RefreshInterval)  // 5 minutes
        defer ticker.Stop()
        for {
            select {
            case <-m.stop:
                return
            case <-ticker.C:  // First check is 5 minutes AFTER Start()!
                // ... check expiry ...
            }
        }
    }()
}
```

If the token expires within the first 5 minutes after startup, no proactive refresh occurs.

**BUG 2: Condition blocks refresh for expired tokens**
```go
// oauth.go:154
if remaining < RefreshWindow && remaining > 0 {
    // refresh...
}
```

If `remaining <= 0` (token already expired), the condition fails and no refresh happens.

### Failure Scenario
1. Token expires at 17:58
2. Ticker fires at 17:55 → remaining = 3 min → **should** trigger refresh (BUG: it should!)
3. But if ticker fires at 18:01 → remaining = -3 min → `remaining > 0` is false → **no refresh**

### Why It Still "Works"
- The reactive path catches expired tokens (401 → refresh → retry)
- But this means every API call fails once before succeeding
- Logs show 401 errors instead of seamless proactive refresh

---

## 5. Data Flow Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                         STARTUP                                 │
├─────────────────────────────────────────────────────────────────┤
│  main.go                                                        │
│    ├── config.Load() → credentials_file = "~/.claude/..."       │
│    ├── NewOAuthManager(credPath)                                │
│    │     └── readCredentials() → accessToken, refreshToken,    │
│    │                            expiresAt from JSON             │
│    ├── mgr.Start() → spawn background ticker goroutine          │
│    ├── NewClientWithTokenFunc(mgr.Token)                        │
│    └── client.SetRefreshFunc(mgr.RefreshIfNeeded)               │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    PROACTIVE REFRESH                            │
├─────────────────────────────────────────────────────────────────┤
│  Background goroutine (oauth.go:140)                            │
│    ├── ticker.C fires every 5 minutes                           │
│    ├── remaining = time.Until(expiresAt)                        │
│    ├── if remaining < 30min && remaining > 0:                   │
│    │     └── refresh() → POST /api/oauth/token                  │
│    │           └── Update accessToken, refreshToken, expiresAt  │
│    │           └── writeCredentials() → update JSON file        │
│    └── (BUG: no check if remaining <= 0)                        │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┤
│                    API CALL (client.go)                         │
├─────────────────────────────────────────────────────────────────┤
│  SendMessage(ctx, req)                                          │
│    ├── tokenBefore = c.getToken()  → mgr.Token()                │
│    ├── sendOnce() → Authorization: Bearer <token>               │
│    ├── if 401 && refreshFunc != nil:                            │
│    │     ├── refreshFunc(tokenBefore) → mgr.RefreshIfNeeded()   │
│    │     │     └── if token changed or refreshing: return nil   │
│    │     │     └── else: refresh() → POST /api/oauth/token      │
│    │     └── retry sendOnce() with new token                    │
│    └── return response or error                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 6. Code References

| Component | File | Line |
|-----------|------|------|
| OAuthManager struct | anthropic/oauth.go | 47-61 |
| NewOAuthManager | anthropic/oauth.go | 89-107 |
| Token() getter | anthropic/oauth.go | 110-114 |
| Start() background ticker | anthropic/oauth.go | 140-163 |
| refresh() API call | anthropic/oauth.go | 172-243 |
| readCredentials | anthropic/oauth.go | 246-289 |
| writeCredentials | anthropic/oauth.go | 294-359 |
| RefreshIfNeeded (reactive) | anthropic/oauth.go | 120-137 |
| Client.getToken | anthropic/client.go | 71-76 |
| Client 401 retry | anthropic/client.go | 204-217 |
| SetRefreshFunc | anthropic/client.go | 79-81 |
| main.go OAuthManager setup | main.go | 231-253 |
| main.go UsageClient | main.go | 464-487 |
| Config credentials_file | config/config.go | 78 |

---

## 7. Recommendations for Fixing TODO #125

### Fix 1: Immediate Check at Startup
Add an immediate expiry check when `Start()` is called, before waiting for the first ticker:

```go
func (m *OAuthManager) Start() {
    // Immediate check on startup
    m.maybeRefresh()
    
    go func() {
        defer close(m.done)
        ticker := time.NewTicker(RefreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-m.stop:
                return
            case <-ticker.C:
                m.maybeRefresh()
            }
        }
    }()
}

func (m *OAuthManager) maybeRefresh() {
    m.mu.Lock()
    remaining := time.Until(m.expiresAt)
    m.mu.Unlock()

    if remaining < RefreshWindow {
        m.logFunc("oauth: token expires in %s, refreshing proactively", remaining.Round(time.Second))
        if err := m.refresh(); err != nil {
            m.logFunc("oauth: proactive refresh failed: %v", err)
        }
    }
}
```

### Fix 2: Handle Already-Expired Tokens
Remove the `remaining > 0` restriction. If the token is already expired, still attempt a refresh (the refresh token should still be valid):

```go
if remaining < RefreshWindow {
    // Refresh even if already expired (remaining <= 0)
    m.logFunc("oauth: token expires in %s, refreshing proactively", remaining.Round(time.Second))
    // ...
}
```

### Fix 3: Add Startup Log
Log the initial token expiry state to help debug future issues:

```go
func (m *OAuthManager) Start() {
    m.mu.Lock()
    remaining := time.Until(m.expiresAt)
    m.mu.Unlock()
    
    m.logFunc("oauth: token expires at %s (in %s)", 
        m.expiresAt.Format(time.RFC3339), 
        remaining.Round(time.Second))
    
    // ... rest of Start()
}
```

---

## 8. Recent OAuth-Related Commits

| Commit | Description |
|--------|-------------|
| `28870d20` | fix(oauth): scopes field is array, not string |
| `c71bd301` | fix(oauth): parse expiresAt as Unix milliseconds, not RFC3339 string |
| `9f965af6` | feat(anthropic): OAuth token auto-refresh with proactive and reactive renewal |

The `c71bd301` fix was critical — without it, `expiresAt` would parse as 0 (zero value for int64), causing `time.UnixMilli(0)` = 1970-01-01, which would always trigger proactive refresh. After the fix, the expiry time is correctly parsed.

---

## 9. Testing the Fix

After applying fixes, verify with:

1. **Unit test for immediate check:**
```go
func TestOAuthManagerImmediateRefresh(t *testing.T) {
    // Token expires in 10 minutes (< 30 min window)
    // Start() should immediately refresh without waiting for ticker
}
```

2. **Unit test for already-expired token:**
```go
func TestOAuthManagerRefreshExpiredToken(t *testing.T) {
    // Token expired 5 minutes ago
    // Start() should still attempt refresh
}
```

3. **Manual test:**
   - Set credentials file with token expiring in 2 minutes
   - Start application
   - Verify log shows immediate refresh attempt
   - Verify API calls work after refresh
