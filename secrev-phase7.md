# Foci Security Review - Phase 7: Authentication & Authorization

**Review Date:** 2026-03-08
**Phase:** 7 of 10
**Status:** Complete

---

## Executive Summary

Phase 7 analyzed all authentication and authorization mechanisms in Foci. The system uses multiple authentication layers: Telegram user allowlisting, HTTP API keys, provider API keys, and OAuth tokens.

**Key Findings:**
- Telegram authentication trusts platform completely (no message signing)
- HTTP API keys have good crypto (constant-time comparison) but weak transport (query params)
- OAuth token management robust with automatic refresh
- No session binding allows token reuse across sessions
- Provider API keys stored in plaintext in memory
- No rate limiting on authentication attempts

**Overall Security Grade:** **B** (Good crypto, weak transport, missing rate limiting)

---

## 1. Telegram User Authentication

### 1.1 User Allowlisting

**Implementation:** `internal/telegram/bot.go:136-167, 706`

**User Validation:**
```go
func NewBot(token string, allowedUsers []string, ...) (*Bot, error) {
    allowed := make(map[string]bool, len(allowedUsers))
    for _, u := range allowedUsers {
        allowed[u] = true
    }
    
    return &Bot{
        allowedUsers: allowed,
        // ...
    }, nil
}

// In message handling
userID := strconv.FormatInt(msg.From.Id, 10)
if !b.allowedUsers[userID] {
    b.logger().Warnf("ignoring message from unauthorized user %s", userID)
    return
}
```

**Strengths:**
✅ **Explicit allowlist** - Only approved users can interact
✅ **User ID-based** - Can't be spoofed (Telegram platform guarantee)
✅ **Simple implementation** - Easy to understand and verify
✅ **Per-agent configuration** - Different users for different agents

**Concerns:**
⚠️ **Trusts Telegram platform** - No additional verification
⚠️ **No message signing** - Trusts message metadata completely
⚠️ **No replay protection** - Same message could be replayed
⚠️ **Account hijacking** - If Telegram account compromised, full access granted
⚠️ **No 2FA option** - Single-factor authentication only

**Attack Vectors:**
1. **Telegram account compromise** - Attacker gains full access
2. **Telegram platform breach** - Platform issues could affect all users
3. **Social engineering** - Attacker convinces admin to add user ID

**Security Grade:** **B-** (Good allowlist, no additional verification)

### 1.2 User ID Format

**Implementation:** String-based user IDs

**Pattern:**
```go
userID := strconv.FormatInt(msg.From.Id, 10) // "123456789"
```

**Strengths:**
✅ **String comparison** - Simple, reliable
✅ **Integer to string** - No encoding issues

**Concerns:**
⚠️ **No validation** - Doesn't verify ID format
⚠️ **No range check** - Could be any integer
⚠️ **String-based map** - Slightly slower than int keys

**Security Grade:** **B+** (Simple and effective)

### 1.3 Multi-Agent Access Control

**Implementation:** Per-agent allowed_users

**Pattern:**
```toml
[[agents]]
id = "agent1"
allowed_users = ["123456789", "987654321"]

[[agents]]
id = "agent2"
allowed_users = ["123456789"] # Only user 123 can access agent2
```

**Strengths:**
✅ **Per-agent isolation** - Different users for different agents
✅ **Flexible configuration** - Fine-grained access control
✅ **Simple model** - Easy to understand

**Concerns:**
⚠️ **No role-based access** - No admin vs regular user distinction
⚠️ **No permission model** - All or nothing access
⚠️ **Manual management** - No self-service user management

**Security Grade:** **B** (Good isolation, no roles)

---

## 2. HTTP API Key Authentication

### 2.1 Key Generation

**Implementation:** `internal/secrets/secrets.go:22-36`

**Algorithm:**
```go
func GeneratePassphrase(wordCount int) (string, error) {
    n := big.NewInt(int64(len(effShortWordlist)))
    words := make([]string, wordCount)
    for i := range words {
        idx, err := rand.Int(rand.Reader, n) // crypto/rand
        if err != nil {
            return "", err
        }
        words[i] = effShortWordlist[idx.Int64()]
    }
    return strings.Join(words, "-"), nil
}
```

**Example:** `"maple-thunder-basket-olive-crane"` (5 words)

**Entropy:**
- Wordlist size: 1,296 words
- Bits per word: log₂(1296) ≈ 10.34 bits
- 5 words: ~52 bits of entropy

**Strengths:**
✅ **Cryptographic RNG** - Uses crypto/rand
✅ **Human-readable** - Easy to type, remember
✅ **Auto-generated** - Created on first startup if missing
✅ **EFF wordlist** - Well-designed wordlist

**Concerns:**
⚠️ **Only 52 bits** - Sufficient but not ideal
⚠️ **Predictable pattern** - Always 5 lowercase words separated by hyphens
⚠️ **No rotation** - Key never changes unless manually rotated
⚠️ **Word list public** - Attacker knows wordlist

**Attack Vectors:**
1. **Brute force** - 2⁵² combinations (expensive but feasible with resources)
2. **Dictionary attack** - Try common word combinations
3. **Key theft** - Steal from config file or environment

**Security Grade:** **B** (Good for most uses, could be stronger)

### 2.2 Key Validation

**Implementation:** `cmd/foci-gw/http.go:71-96`

**Pattern:**
```go
func authMiddleware(apiKey string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Check Authorization: Bearer header
        token := ""
        if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
            token = auth[len("Bearer "):]
        }
        
        // Fallback: api_key query param
        if token == "" {
            token = r.URL.Query().Get("api_key")
        }
        
        if token == "" {
            http.Error(w, "authentication required", http.StatusUnauthorized)
            return
        }
        
        // Constant-time comparison
        if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
            http.Error(w, "invalid credentials", http.StatusForbidden)
            return
        }
        
        next.ServeHTTP(w, r)
    })
}
```

**Strengths:**
✅ **Constant-time comparison** - Prevents timing attacks
✅ **Standard Bearer header** - Follows RFC 6750
✅ **Multiple auth methods** - Header or query parameter
✅ **Clear error messages** - "authentication required" vs "invalid credentials"

**Concerns:**
⚠️ **Query parameter exposure** - Token logged in access logs, browser history
⚠️ **No rate limiting** - Unlimited authentication attempts
⚠️ **No token expiration** - Keys never expire
⚠️ **No revocation mechanism** - Can't revoke compromised keys
⚠️ **No session binding** - Same key works for all sessions/agents

**Attack Vectors:**
1. **Brute force** - Unlimited attempts (though 52-bit entropy makes this hard)
2. **Log harvesting** - Extract keys from access logs
3. **Browser history** - Keys visible in history if query param used
4. **Key sharing** - No way to track or limit key usage

**Security Grade:** **B-** (Good crypto, weak transport)

### 2.3 Key Storage

**Implementation:** `secrets.toml`

**Storage:**
```toml
[http]
api_key = "maple-thunder-basket-olive-crane"
```

**File Permissions:** root:foci-secrets 0660 (OS-protected)

**Strengths:**
✅ **OS-level protection** - Group-based access control
✅ **Gitignored by default** - Not committed to repository
✅ **Auto-generated** - No manual creation needed

**Concerns:**
⚠️ **Plaintext** - Not encrypted at rest
⚠️ **In memory** - Loaded into process memory
⚠️ **No rotation** - Static unless manually changed

**Security Grade:** **B+** (Good OS protection, no encryption)

---

## 3. Provider API Key Management

### 3.1 Anthropic API Keys

**Implementation:** `internal/anthropic/client.go`

**Key Types:**
1. **API Keys** - Static keys (sk-ant-...)
2. **OAuth Tokens** - Dynamic tokens with refresh

**Storage:**
```toml
[anthropic]
api_key = "sk-ant-..."  # OR
setup_token = "sk-ant-oat01-..."  # Converted to OAuth
```

**Strengths:**
✅ **Lazy initialization** - Client created on demand
✅ **Per-endpoint clients** - Separate for different models
✅ **OAuth support** - Automatic token refresh

**Concerns:**
⚠️ **Plaintext in memory** - Keys stored in client structs
⚠️ **No encryption** - Not encrypted at rest or in memory
⚠️ **No rotation** - Manual rotation only
⚠️ **Long-lived** - API keys don't expire

**Security Grade:** **B-** (Functional, no encryption)

### 3.2 Gemini API Keys

**Implementation:** `internal/gemini/client.go`

**Storage:**
```toml
[gemini]
api_key = "AIza..."
```

**Strengths:** Same as Anthropic

**Concerns:** Same as Anthropic

**Security Grade:** **B-** (Same as Anthropic)

### 3.3 OpenAI API Keys

**Implementation:** `internal/openai/client.go`

**Storage:**
```toml
[openai]
api_key = "sk-..."
```

**Strengths:** Same as Anthropic

**Concerns:** Same as Anthropic

**Security Grade:** **B-** (Same as Anthropic)

---

## 4. OAuth Token Management

### 4.1 OAuth Flow

**Implementation:** `internal/anthropic/oauth.go`

**Flow:**
1. User runs `foci auth`
2. Browser opens Claude authorization page
3. User authorizes, receives setup token
4. Setup token exchanged for access + refresh tokens
5. Tokens stored in secrets.toml
6. Background goroutine refreshes access token before expiry

**Token Structure:**
```json
{
  "access_token": "...",
  "refresh_token": "...",
  "expires_at": 1234567890000
}
```

**Strengths:**
✅ **Automatic refresh** - Tokens refreshed before expiry
✅ **Background goroutine** - No user intervention needed
✅ **Refresh token** - Long-lived refresh token (365 days)
✅ **Secure storage** - OS-protected secrets.toml

**Concerns:**
⚠️ **Plaintext storage** - Tokens not encrypted
⚠️ **In memory** - Tokens loaded into process memory
⚠️ **No revocation** - Can't revoke refresh token from app
⚠️ **Long-lived refresh** - 365-day refresh token lifetime

**Security Grade:** **B+** (Good auto-refresh, no encryption)

### 4.2 Token Refresh

**Implementation:** `internal/anthropic/oauth.go:159-185`

**Pattern:**
```go
func (m *OAuthManager) Refresh() error {
    m.mu.RLock()
    refreshToken := m.creds.RefreshToken
    m.mu.RUnlock()
    
    newCreds, err := refreshAccessToken(m.httpClient, m.tokenURL, refreshToken)
    if err != nil {
        return err
    }
    
    m.mu.Lock()
    m.creds = *newCreds
    m.mu.Unlock()
    
    if !m.readOnly && m.store != nil {
        return saveCredsToStore(m.store, newCreds)
    }
    return nil
}
```

**Strengths:**
✅ **Thread-safe** - Mutex-protected
✅ **Atomic update** - All fields updated together
✅ **Persisted** - Saved to secrets.toml
✅ **Error handling** - Graceful failure

**Concerns:**
⚠️ **No refresh token rotation** - Same refresh token used repeatedly
⚠️ **No circuit breaker** - Failed refreshes don't back off
⚠️ **No monitoring** - Can't track refresh failures

**Security Grade:** **B+** (Good implementation, no rotation)

### 4.3 Claude Code Credentials Fallback

**Implementation:** `internal/anthropic/oauth.go:87-113`

**Mechanism:**
- Reads `~/.claude/.credentials.json`
- Extracts OAuth tokens
- Read-only mode (doesn't write back on refresh)

**Strengths:**
✅ **Convenient** - Works with existing Claude Code setup
✅ **Read-only** - Doesn't modify Claude Code files
✅ **Fallback** - Works without foci-native credentials

**Concerns:**
⚠️ **External file** - Reads from user home directory
⚠️ **Permission dependency** - Relies on file permissions
⚠️ **No validation** - Doesn't verify file ownership

**Security Grade:** **B** (Convenient but external dependency)

---

## 5. Bitwarden Authentication

### 5.1 Two-Tier Approval Model

**Implementation:** `internal/secrets/bitwarden/bitwarden.go`

**Tier 1: Metadata (Auto-approved)**
- Command: `sudo -u bitwarden bw list items`
- Returns: Item names, URIs, folders (NO passwords)
- Security: Allowlisted in aisudo

**Tier 2: Passwords (Approval-required)**
- Command: `sudo -u bitwarden bw get password <id>`
- Returns: Actual password value
- Security: Requires Telegram approval

**Strengths:**
✅ **Human approval gate** - Password access requires explicit approval
✅ **Dedicated user** - Runs as bitwarden system user
✅ **Session isolation** - Bitwarden user reads own session file
✅ **TTL-based caching** - Passwords expire from cache
✅ **Audit trail** - Approval requests logged

**Concerns:**
⚠️ **Approval fatigue** - Users may auto-approve without reading
⚠️ **Session token in file** - Bitwarden session stored in file
⚠️ **No multi-factor** - Single approval only
⚠️ **Social engineering** - Attacker could request approval for malicious use

**Security Grade:** **A-** (Strong design, human factor risks)

### 5.2 Session Management

**Implementation:**
```bash
# Session file read by bitwarden user
export BW_SESSION=$(cat /home/bitwarden/.bw_session) && bw get password <id>
```

**Strengths:**
✅ **Process isolation** - foci never sees session token
✅ **Dedicated user** - bitwarden user owns session
✅ **File permissions** - Session file protected

**Concerns:**
⚠️ **File-based** - Session token in file (could be read if permissions wrong)
⚠️ **No expiration** - Session could be long-lived
⚠️ **No rotation** - Session not rotated

**Security Grade:** **B+** (Good isolation, file-based)

---

## 6. MCP Server Authentication

### 6.1 Stdio MCP Servers

**Implementation:** `internal/mcp/transport_stdio.go`

**Mechanism:**
- Process spawned via exec.Command
- stdin/stdout JSON-RPC
- No network authentication

**Strengths:**
✅ **No network exposure** - Local process only
✅ **Process isolation** - Separate process with group dropping
✅ **Simple** - No auth complexity

**Concerns:**
⚠️ **No server authentication** - Trusts any server that responds
⚠️ **No message signing** - Messages not authenticated
⚠️ **Process trust** - Trusts server process completely

**Security Grade:** **B** (Good isolation, no authentication)

### 6.2 HTTP MCP Servers

**Implementation:** `internal/mcp/transport_http.go`

**Mechanism:**
- HTTP client connects to server
- JSON-RPC over HTTP
- No authentication headers

**Strengths:**
✅ **Simple** - Standard HTTP client

**Concerns:**
⚠️ **No authentication** - No API key or token
⚠️ **No TLS enforcement** - HTTP allowed
⚠️ **Server trust** - Trusts server responses
⚠️ **MITM risk** - No certificate pinning

**Security Grade:** **D** (No authentication)

---

## 7. Session-Based Access Control

### 7.1 Session Key Format

**Implementation:** `internal/session/key.go`

**Format:** `agent:{agentID}:{type}:{identifier}`

**Examples:**
- `agent:main:main` - Default session
- `agent:main:chat:123456789` - Telegram chat session
- `agent:main:spawn:spawn-1234567890` - Spawned session

**Strengths:**
✅ **Structured format** - Clear namespacing
✅ **Agent isolation** - Agent ID in key
✅ **Type separation** - Different types isolated

**Concerns:**
⚠️ **Predictable** - Format is known
⚠️ **No secret** - No random component (except spawn timestamp)
⚠️ **No signature** - Not cryptographically signed
⚠️ **Enumeration** - Could enumerate sessions

**Security Grade:** **C** (Structured but predictable)

### 7.2 Session Access Control

**Implementation:** None

**Current Behavior:**
- API key grants access to ALL sessions
- No per-session authentication
- No session ownership tracking

**Concerns:**
⚠️ **No session binding** - API key not bound to specific session
⚠️ **Cross-session access** - Single key accesses all sessions
⚠️ **No session ACLs** - No fine-grained permissions
⚠️ **Session hijacking** - Knowing session key grants access

**Security Grade:** **D** (No session-level auth)

---

## 8. Critical Findings - Phase 7

### Finding 7.1: No Authentication Rate Limiting (MEDIUM)

**Location:** All authentication endpoints
**Issue:** Unlimited authentication attempts allowed
**Impact:** Brute force attacks (though expensive with 52-bit keys)
**Recommendation:**
```go
import "golang.org/x/time/rate"

var authLimiter = rate.NewLimiter(rate.Every(time.Second), 5) // 5 req/s

func authMiddleware(apiKey string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !authLimiter.Allow() {
            http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
            return
        }
        // ... existing auth logic
    })
}
```

### Finding 7.2: Query Parameter Auth Logging (MEDIUM)

**Location:** `cmd/foci-gw/http.go:79-82`
**Issue:** API key can be passed in URL, logged in access logs
**Impact:** Key exposure through logs, browser history
**Recommendation:**
- Deprecate query parameter auth
- Add warning header when query param used
- Log redaction for api_key parameters

### Finding 7.3: No API Key Rotation (LOW)

**Location:** API key management
**Issue:** Keys never expire or rotate automatically
**Impact:** Long-lived keys increase exposure window
**Recommendation:**
- Add optional key expiration
- Implement key rotation mechanism
- Add key usage tracking

### Finding 7.4: No Session Binding (MEDIUM)

**Location:** HTTP API authentication
**Issue:** API key not bound to specific session or agent
**Impact:** Single compromised key accesses all sessions
**Recommendation:**
```go
type SessionBoundKey struct {
    Key        string
    AgentID    string
    SessionKey string
    ExpiresAt  time.Time
}
```

### Finding 7.5: Provider Keys in Plaintext (MEDIUM)

**Location:** All provider clients
**Issue:** API keys stored in plaintext in memory
**Impact:** Memory dump exposes all keys
**Recommendation:**
- Encrypt keys in memory with session key
- Use secure memory (mlock)
- Zero memory after use

### Finding 7.6: Telegram Platform Trust (LOW)

**Location:** `internal/telegram/bot.go`
**Issue:** Trusts Telegram's user ID without additional verification
**Impact:** Telegram account compromise grants full access
**Recommendation:**
- Add optional 2FA for sensitive operations
- Implement session timeouts
- Add anomaly detection

### Finding 7.7: MCP HTTP No Authentication (MEDIUM)

**Location:** `internal/mcp/transport_http.go`
**Issue:** HTTP MCP servers have no authentication
**Impact:** Anyone can connect to MCP server
**Recommendation:**
- Add API key authentication for MCP servers
- Or use TLS client certificates
- Or validate server identity

### Finding 7.8: OAuth No Refresh Token Rotation (LOW)

**Location:** `internal/anthropic/oauth.go`
**Issue:** Same refresh token used repeatedly
**Impact:** Stolen refresh token remains valid
**Recommendation:**
- Implement refresh token rotation
- Invalidate old refresh token on use
- Add refresh token usage tracking

---

## 9. Authentication Security Matrix

| Mechanism | Rate Limit | Expiration | Rotation | Encryption | Binding | Grade |
|-----------|------------|------------|----------|------------|---------|-------|
| **Telegram Allowlist** | ❌ None | ❌ Never | ❌ Manual | ❌ None | ⚠️ Platform | B- |
| **HTTP API Key** | ❌ None | ❌ Never | ❌ Manual | ❌ Plaintext | ❌ None | B- |
| **Anthropic API** | ❌ None | ❌ Never | ❌ Manual | ❌ Plaintext | ❌ None | B- |
| **Anthropic OAuth** | ❌ None | ✅ 1hr | ✅ Auto | ❌ Plaintext | ❌ None | B+ |
| **Bitwarden** | ❌ None | ✅ TTL | ✅ Manual | ❌ Plaintext | ⚠️ Approval | A- |
| **MCP Stdio** | N/A | N/A | N/A | N/A | ❌ None | B |
| **MCP HTTP** | ❌ None | ❌ Never | ❌ Manual | ❌ None | ❌ None | D |

---

## 10. Authorization Summary

### Strong Controls:
1. ✅ Telegram user allowlisting
2. ✅ HTTP API key constant-time comparison
3. ✅ OAuth automatic token refresh
4. ✅ Bitwarden two-tier approval
5. ✅ Per-agent user isolation

### Weak Controls:
1. ❌ No authentication rate limiting
2. ❌ No API key expiration/rotation
3. ❌ No session binding
4. ❌ Keys in plaintext memory
5. ❌ Query parameter auth logging

### Missing Controls:
1. ❌ No role-based access control
2. ❌ No permission model
3. ❌ No session-level authentication
4. ❌ No audit logging for auth events
5. ❌ No anomaly detection

---

## 11. Recommendations Priority

### Critical Priority:
1. **Add authentication rate limiting** (Finding 7.1)
2. **Implement session binding** (Finding 7.4)

### High Priority:
3. **Deprecate query parameter auth** (Finding 7.2)
4. **Add MCP authentication** (Finding 7.7)
5. **Encrypt keys in memory** (Finding 7.5)

### Medium Priority:
6. **Implement key rotation** (Finding 7.3)
7. **Add 2FA for sensitive operations** (Finding 7.6)
8. **Implement refresh token rotation** (Finding 7.8)

---

## 12. Comparison to Industry Standards

**Better Than:**
- Simple password-only systems
- Systems without constant-time comparison

**On Par With:**
- API key authentication (standard)
- OAuth 2.0 implementation (standard)

**Lags Behind:**
- Enterprise systems (no RBAC, no audit logging)
- High-security systems (no HSM, no key rotation)
- Modern web apps (no session binding)

---

**Phase 7 Status:** ✅ COMPLETE
**Next Phase:** Phase 8 - Process Isolation & Privilege Management
