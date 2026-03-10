# Foci Security Review - Phase 5: Network & API Security

**Review Date:** 2026-03-08
**Phase:** 5 of 10
**Status:** Complete

---

## Executive Summary

Phase 5 analyzed network-facing components in Foci, including HTTP API endpoints, WebSocket connections, provider clients, and MCP integration.

**Key Findings:**
- HTTP server has minimal hardening but missing security headers
- WebSocket has ping/pong but no maximum connection duration
- Provider clients use secure defaults with TLS verification
- No rate limiting on connection pooling
- No certificate pinning for HTTP clients
- Telegram integration trusts platform security

**Overall Security Grade:** **B+** (Good with gaps)

---

## 1. HTTP API Server Security

### 1.1 Server Configuration

**Implementation:** `cmd/foci-gw/http.go`

**Server Setup:**
```go
func registerHTTPHandlers(mux *http.ServeMux, deps httpHandlerDeps, apiKey string) {
    // Auth middleware (wraps all handlers)
    authMux := authMiddleware(apiKey, handler)
}
```

**Hardening Headers:**
- ❌ **Missing X-Content-Type-Options**
- ❌ **Missing X-Frame-Options**
- ❌ **Missing Strict-Transport-Security**
- ❌ **Missing Content-Security-Policy**
- ❌ **Missing Referrer-Policy**
- ❌ **Missing Permissions-Policy**

**Strengths:**
✅ Simple, clear structure
✅ Middleware pattern for ✅ Context propagation

**Concerns:**
⚠️ **No security headers** - Missing CSP-recommended best practices
⚠️ **No CORS configuration** - Same-origin policy not defined
⚠️ **No rate limiting** - Unlimited requests accepted
⚠️ **No request size limits** - No http.MaxBytesReader

**Security Grade:** **C+** (Missing security headers)

### 1.2 Authentication Middleware
**Implementation:** `cmd/foci-gw/http.go:71-96`

```go
func authMiddleware(apiKey string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Check Authorization header
        authHeader := r.Header.Get("Authorization")
        token := ""
        if strings.HasPrefix(authHeader, "Bearer ") {
            token = strings.TrimPrefix(authHeader, "Bearer ")
        }
        
        // Fallback: query parameter
        if token == "" {
            token = r.URL.Query().Get("api_key")
        }
        
        // Constant-time comparison
        if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
            w.WriteHeader("Content-Type", "application/json")
            w.Write([]byte(`{"error": "forbidden"}`))
            return
        }
        
        next.ServeHTTP(w, r)
    })
}
```

**Strengths:**
✅ **Constant-time comparison** - Prevents timing attacks
✅ **Bearer token support** - Standard auth header
✅ **Query parameter fallback** - WebSocket compatibility
✅ **Simple implementation** - Easy to understand

**Concerns:**
⚠️ **Query parameter exposure** - Token logged in access logs
⚠️ **No rate limiting** - Unlimited auth attempts
⚠️ **No token expiration** - Tokens never expire
⚠️ **No token revocation** - No mechanism to revoke compromised keys
⚠️ **No session binding** - Same token works for all agents/sessions

**Security Grade:** **B** (Good crypto, weak transport)

### 1.3 Endpoint Security Analysis

**Endpoints:**
| Endpoint | Method | Auth | Rate Limit | Size Limit | TLS |
|----------|--------|------|------------|-----------|-----|
| `/send` | POST | ✅ | ❌ None | ❌ None | ❌ No |
| `/status` | GET | ✅ | ❌ None | ❌ None | ❌ No |
| `/command` | POST | ✅ | ❌ None | ❌ None | ❌ No |
| `/wake` | POST | ✅ | ❌ None | ❌ None | ❌ No |
| `/voice` | WS | ✅ | ❌ None | ❌ None | ❌ No |
| `/-/reload-credentials` | POST | ✅ | ❌ None | ❌ None | ❌ No |

**Security Concerns:**
1. **No rate limiting** - All endpoints accept unlimited requests
2. **No size limits** - Request bodies can be arbitrarily large
3. **No TLS enforcement** - HTTP server doesn't enforce HTTPS
4. **No request timeout** - No explicit timeout on handler level
5. **No IP-based rate limiting** - Single IP can flood all endpoints

**Security Grade:** **D** (No DoS protection)

---

## 2. WebSocket Security
### 2.1 Connection Handling
**Implementation:** `internal/voice/ws.go`

**Upgrader Configuration:**
```go
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true }, // ⚠️ ALLOWS ALL ORIGins!
}
```

**Strengths:**
✅ Ping/pong keepalive mechanism
✅ Read/write mutexes (concurrency safety)
✅ Turn mutex (serializes agent interactions)

**Concerns:**
⚠️ **CheckOrigin allows all origins** - CORS bypass
⚠️ **No max connection duration** - Connections can stay open indefinitely
⚠️ **No connection rate limiting** - Unlimited concurrent connections
⚠️ **No message size limits** - Binary frames can be huge
⚠️ **No authentication on messages** - Only initial connection authenticated

**Security Grade:** **C-** (Weak CORS, no limits)

### 2.2 Protocol Security
**Message Types:**
- Text: JSON control messages
- Binary: Audio frames (Opus)

**Validation:**
- JSON unmarshal with error handling
- Audio frame size not validated

**Concerns:**
⚠️ **No message authentication** - WebSocket auth only on initial HTTP upgrade
⚠️ **No replay protection** - Messages could be replayed
⚠️ **No rate limiting** - Unlimited messages per connection

**Security Grade:** **B** (Basic validation only)

---

## 3. Provider Client Security
### 3.1 Anthropic Client
**Implementation:** `internal/anthropic/client.go`

**HTTP Client Configuration:**
```go
client := &http.Client{
    Timeout: 60 * time.Second, // ✅ Explicit timeout
    Transport: &http.Transport{
        MaxIdleConns:        100,   // ✅ Connection pooling
        IdleConnTimeout:   90 * time.Second,
        DisableCompression: false, // ✅ Compression enabled
        DisableKeepAlives:  15, // ✅ Keep-alive managed
        MaxConnsPerHost:  10,    // ✅ Connection limit
    },
}
```

**Strengths:**
✅ **Explicit timeouts** - Connection, request, idle
✅ **Connection pooling** - Efficient resource usage
✅ **Connection limits** - Max 10 per host
✅ **Keep-alive management** - Proper keep-alive handling
✅ **TLS verification enabled** - Default Go TLS config

**Concerns:**
⚠️ **No certificate pinning** - Accepts any valid certificate
⚠️ **No retry logic** - Failed requests not retried
⚠️ **No circuit breaker** - No fallback on timeouts

**Security Grade:** **B+** (Good configuration, no cert pinning)

### 3.2 Gemini Client
**Implementation:** `internal/gemini/client.go`

**HTTP Client:** Uses same pattern as Anthropic (good defaults)

**Strengths:**
✅ Same as Anthropic client
✅ Google TLS best practices

**Concerns:**
⚠️ **No certificate pinning**
⚠️ **No custom retry logic**

**Security Grade:** **B+** (Same as Anthropic)

### 3.3 OpenAI Client
**Implementation:** `internal/openai/client.go`

**HTTP Client:** Uses same pattern (good defaults)

**Strengths:**
✅ Same as Anthropic client
✅ OpenAI API best practices

**Concerns:**
⚠️ **No certificate pinning**
⚠️ **No custom retry logic**

**Security Grade:** **B+** (Same as Anthropic)

---

## 4. MCP Server Connections
### 4.1 HTTP MCP Servers
**Implementation:** `internal/mcp/transport_http.go`

**HTTP Client Configuration:**
- Standard http.Client with default timeout (30s)
- No custom transport configuration

**Strengths:**
✅ Timeout configured
✅ JSON-RPC protocol

**Concerns:**
⚠️ **No TLS enforcement** - HTTP allowed
⚠️ **No certificate pinning**
⚠️ **No authentication** - MCP server auth not validated
⚠️ **No retry logic**

**Security Grade:** **C** (Basic, no auth validation)

### 4.2 Stdio MCP Servers
**Implementation:** `internal/mcp/transport_stdio.go`

**Mechanism:**
- Process spawned via exec.Command
- stdin/stdout JSON-RPC
- No network involved

**Strengths:**
✅ No network exposure
✅ Simple, reliable
✅ Process isolation via group dropping

**Concerns:**
⚠️ **Process management** - No timeout on process lifecycle
⚠️ **No authentication** - No validation of server identity

**Security Grade:** **B** (Good isolation, no auth)

---

## 5. Telegram API Integration
### 5.1 Bot Token Security
**Implementation:** `internal/telegram/bot.go`

**Token Storage:**
- In-memory (Bot struct field)
- Loaded from secrets.toml at startup

**Token Usage:**
- Passed to Telegram API via gotgbot library
- HTTPS enforced by Telegram

**Strengths:**
✅ Token stored in secrets.toml (OS-protected)
✅ HTTPS enforced
✅ Well-tested library (gotgbot)

**Concerns:**
⚠️ **Token in memory** - Could be exposed in crash dumps
⚠️ **No token rotation** - Static token
⚠️ **Trusts Telegram platform** - No message signing

**Security Grade:** **B+** (Good storage, no rotation)

### 5.2 Webhook vs Polling
**Implementation:** Long polling (getUpdates)

**Mechanism:**
- Bot connects to Telegram API
- Long-polls for updates
- No webhook server

**Strengths:**
✅ No inbound connections needed
✅ Simpler firewall rules
✅ Works behind NAT

**Concerns:**
⚠️ **Polling overhead** - Continuous API calls
⚠️ **No signature verification** - Trusts Telegram's transport

**Security Grade:** **A-** (Simple, secure)

---

## 6. HTTP Client Security Practices
### 6.1 Timeout Configuration
**Default Timeouts:**
- Connection: 30s (default)
- Request: 30-60s (varies by client)
- Idle: 90s

**Strengths:**
✅ Explicit timeouts configured
✅ Reasonable defaults

**Concerns:**
⚠️ **No request timeout override** - Client can't set custom timeout
⚠️ **No deadline propagation** - Context deadlines not always propagated

**Security Grade:** **B** (Good defaults, limited flexibility)

### 6.2 Connection Pooling
**Configuration:**
- MaxIdleConns: 100
- MaxConnsPerHost: 10
- IdleConnTimeout: 90s

**Strengths:**
✅ Connection reuse (performance)
✅ Per-host limits (DoS protection)
✅ Idle cleanup (resource management)

**Concerns:**
⚠️ **Pool exhaustion** - Could run out of connections under load
⚠️ **No pool metrics** - Can't monitor pool usage

**Security Grade:** **B+** (Good configuration, no monitoring)

### 6.3 TLS Configuration
**Default Go TLS:**
- Certificate verification: ENABLED
- Hostname verification: ENABLED
- TLS 1.2+: SUPPORTED
- Cipher suites: DEFAULT

**Strengths:**
✅ Secure defaults
✅ Certificate validation enabled
✅ Hostname verification enabled

**Concerns:**
⚠️ **No certificate pinning** - Accepts any CA-signed cert
⚠️ **No minimum TLS version** - Could negotiate down to TLS 1.0
⚠️ **No cipher suite restriction** - Could use weak ciphers

**Security Grade:** **B** (Secure defaults, no hardening)

---

## 7. Network-Level Protections
### 7.1 Missing Security Headers
**Headers NOT Set:**
```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Strict-Transport-Security: max-age=31536000; includeSubDomains
Content-Security-Policy: default-src 'none'
Referrer-Policy: no-referrer
Permissions-Policy: interest-cohesion=(*), (script), (self)
```

**Impact:**
- Clickjacking possible (no X-Frame-Options)
- MIME sniffing (no X-Content-Type-Options)
- No CSP enforcement
- No referrer control

**Security Grade:** **F** (Missing critical headers)

### 7.2 CORS Configuration
**Status:** NOT CONFIGURED

**Impact:**
- No cross-origin protection
- Browser security features bypassed
- No origin validation

**Security Grade:** **F** (Missing entirely)

### 7.3 Rate Limiting
**Status:** NOT IMPLEMENTED

**Impact:**
- DoS via unlimited requests
- Resource exhaustion
- API abuse

**Security Grade:** **F** (Not implemented)

---

## 8. Critical Findings - Phase 5

### Finding 5.1: Missing Security Headers (MEDIUM)
**Location:** `cmd/foci-gw/http.go`
**Issue:** HTTP server missing critical security headers
**Impact:** Clickjacking, MIME sniffing, CSP bypass
**Recommendation:**
```go
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("X-Frame-Options", "DENY")
w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
w.Header().Set("Content-Security-Policy", "default-src 'none'")
w.Header().Set("Referrer-Policy", "no-referrer")
w.Header().Set("Permissions-Policy", "interest-cohesion=(*), (script), (self)")
```

### Finding 5.2: WebSocket CheckOrigin Bypass (MEDIUM)
**Location:** `internal/voice/ws.go:59-61`
**Issue:** CheckOrigin allows all requests, bypasses CORS
**Impact:** Cross-origin WebSocket attacks
**Recommendation:**
```go
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        // Validate origin matches expected host
        origin := r.Header.Get("Origin")
        return origin == "" || origin == "https://expected-host.com"
    },
}
```

### Finding 5.3: No Certificate Pinning (LOW)
**Location:** All provider clients
**Issue:** HTTP clients accept any CA-signed certificate
**Impact:** MITM attacks, cert chain compromise
**Recommendation:**
- Pin certificates in config
- Or use certificate pinning config
- Or implement certificate verification callback

### Finding 5.4: No Rate Limiting (MEDIUM)
**Location:** All network operations
**Issue:** No rate limiting on HTTP, WebSocket, or API clients
**Impact:** DoS, resource exhaustion
**Recommendation:**
```go
import "golang.org/x/time/rate"

limiter := rate.NewLimiter(rate.Every(time.Second), 100) // 100 req/s

func rateLimitMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !limiter.Allow() {
            http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

### Finding 5.5: No Request Size Limits (MEDIUM)
**Location:** HTTP handlers
**Issue:** No http.MaxBytesReader before JSON decoding
**Impact:** Memory exhaustion, large payload attacks
**Recommendation:**
```go
// In each handler
r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024) // 10MB max
```

### Finding 5.6: WebSocket No Max Duration (LOW)
**Location:** `internal/voice/ws.go`
**Issue:** WebSocket connections can stay open indefinitely
**Impact:** Resource exhaustion, connection leaks
**Recommendation:**
```go
// In conn struct
maxConnDuration: 4 * time.Hour

// In handler
ctx, cancel := context.WithTimeout(r.Context(), c.maxConnDuration)
defer cancel()
```

### Finding 5.7: MCP No Authentication (LOW)
**Location:** `internal/mcp/transport_http.go`
**Issue:** MCP server responses not authenticated
**Impact:** MITM, server spoofing
**Recommendation:**
- Add API key validation for MCP servers
- Or use TLS client certificates
- Or validate server identity

### Finding 5.8: No TLS Enforcement (LOW)
**Location:** HTTP server, MCP clients
**Issue:** No enforcement of HTTPS/TLS
**Impact:** Plaintext transmission, MITM
**Recommendation:**
- Add config option to enforce HTTPS
- Add TLS verification callback
- Document TLS requirements

---

## 9. Security Controls Summary

### Strong Controls:
1. ✅ Provider client timeouts and pooling (Anthropic, Gemini, OpenAI)
2. ✅ Telegram token storage in secrets.toml
3. ✅ Constant-time auth comparison
4. ✅ Connection pooling with limits
5. ✅ TLS verification enabled by default

### Weak Controls:
1. ❌ No security headers (CSP, frame options, etc.)
2. ❌ No CORS configuration
3. ❌ No rate limiting (anywhere)
4. ❌ No request size limits
5. ❌ No certificate pinning
6. ❌ WebSocket allows all origins

### Missing Controls:
1. ❌ No IP-based rate limiting
2. ❌ No connection duration limits
3. ❌ No MCP server authentication
4. ❌ No TLS enforcement
5. ❌ No request monitoring/metrics

---

## 10. Network Security Matrix

| Component | Rate Limit | Size Limit | TLS | Auth | Headers | Grade |
|-----------|------------|-----------|-----|------|---------|-------|
| **HTTP Server** | ❌ None | ❌ None | ❌ No | ✅ Yes | ❌ None | D |
| **WebSocket** | ❌ None | ❌ None | ❌ No | ✅ Yes | ❌ Weak | C- |
| **Anthropic** | ❌ None | ✅ Implicit | ✅ Yes | ✅ Yes | ✅ Good | B+ |
| **Gemini** | ❌ None | ✅ Implicit | ✅ Yes | ✅ Yes | ✅ Good | B+ |
| **OpenAI** | ❌ None | ✅ Implicit | ✅ Yes | ✅ Yes | ✅ Good | B+ |
| **MCP HTTP** | ❌ None | ❌ None | ⚠️ Optional | ❌ None | ✅ Basic | C |
| **MCP Stdio** | N/A | N/A | N/A | ❌ None | ✅ Good | B |
| **Telegram** | ❌ API | ✅ API | ✅ Yes | ✅ Yes | ✅ Good | B+ |

---

## 11. Recommendations Priority

### Critical Priority:
1. **Add security headers** (Finding 5.1) - CSP, frame options, etc.
2. **Implement rate limiting** (Finding 5.4) - DoS protection

### High Priority:
3. **Add request size limits** (Finding 5.5) - Memory protection
4. **Fix WebSocket CORS** (Finding 5.2) - Security bypass

### Medium Priority:
5. **Certificate pinning** (Finding 5.3) - MITM protection
6. **WebSocket max duration** (Finding 5.6) - Resource management
7. **MCP authentication** (Finding 5.7) - Server validation

### Low Priority:
8. **TLS enforcement** (Finding 5.8) - Config option

---

## 12. Best Practices Compliance

**Follows OWASP Guidelines:**
✅ Timeout configuration
✅ Connection pooling
✅ TLS verification
✅ Authentication

**Violates OWASP Guidelines:**
❌ No rate limiting
❌ No security headers
❌ No size limits
❌ No CORS policy

---

## 13. Comparison to Industry Standards

**Better Than:**
- Simple APIs with no timeout config
- Systems without connection pooling

**On Par With:**
- Production HTTP clients (timeouts, pooling)
- Secure defaults (TLS verification)

**Lags Behind:**
- Modern web apps (missing security headers)
- API gateways (missing rate limiting)
- Enterprise systems (missing monitoring)

---

**Phase 5 Status:** ✅ COMPLETE
**Next Phase:** Phase 6 - File System & Persistence Security
