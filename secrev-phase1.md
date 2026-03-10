# Foci Security Review - Phase 1: Architecture & Threat Model Analysis

**Review Date:** 2026-03-07
**Phase:** 1 of 10
**Status:** Complete

---

## Executive Summary

Foci is a sophisticated multi-agent AI platform written in Go with:
- Multi-provider support (Anthropic, Gemini, OpenAI)
- Telegram bot integration
- HTTP API (REST + WebSocket)
- Advanced secrets management with OS-level protection
- Tool system with file operations, shell execution, HTTP requests

**Critical Finding:** The system implements defense-in-depth security with OS-level group dropping for secrets protection, but the attack surface is extensive with multiple entry points and complex data flows.

---

## 1. Entry Points Mapping

### 1.1 HTTP API Endpoints (Network-Exposed)

**Location:** `cmd/foci-gw/http.go`, `cmd/foci-gw/http_handlers.go`

| Endpoint | Method | Auth Required | Trust Level | Data Flow |
|----------|--------|---------------|-------------|-----------|
| `/send` | POST | ✅ Bearer token or query param | **High** | Direct agent message injection |
| `/status` | GET | ✅ Bearer token or query param | Medium | Status query |
| `/command` | POST | ✅ Bearer token or query param | **High** | Slash command execution |
| `/wake` | POST | ✅ Bearer token or query param | **High** | Wake/branch session creation |
| `/voice` | WebSocket | ✅ Bearer token or query param | **High** | Real-time voice conversation |
| `/-/reload-credentials` | POST | ✅ Bearer token | **Critical** | Hot credential reload |

**Authentication Implementation:**
- Uses `crypto/subtle.ConstantTimeCompare` for timing-safe comparison
- Allows query parameter auth (WebSocket compatibility) - logs/history risk
- No built-in rate limiting on any endpoints
- No explicit request size limits before JSON decoding
- Auto-generated API key with 52-bit entropy (5-word passphrase)

### 1.2 Telegram Bot Integration

**Location:** `internal/telegram/bot.go`

**Entry Flow:**
```
Telegram API → gotgbot long-poll → Bot.Run()
→ check allowed_users (allowlist)
→ Slash command? → command.Dispatch() → immediate execution
→ Regular message → queue → agent worker goroutine
→ Agent processes → send response
```

**Security Concerns:**
- ✅ User allowlist restricts to specific Telegram user IDs
- ⚠️ No message signing verification (trusts Telegram's user ID completely)
- ⚠️ No replay protection
- ⚠️ Bot token stored in memory

### 1.3 WebSocket Voice Interface

**Location:** `internal/voice/ws.go`

**Entry Flow:**
- GET /voice?api_key=KEY → authMiddleware → WebSocket upgrade
- Client selects agent → sends audio frames → receives transcribed response

**Security Concerns:**
- ⚠️ Large binary payloads with no size limits
- ⚠️ No concurrent session limits
- ✅ Per-connection mutexes (writeMu, turnMu, audioMu)
- ⚠️ No session timeout

### 1.4 CLI Commands

**Location:** `cmd/foci/main.go`

**Commands:** send, branch, status, auth, secrets

**Security Concerns:**
- ✅ Uses HTTP API auth (FOCI_API_KEY env or --api-key)
- ⚠️ `foci auth` writes directly to secrets.toml
- ⚠️ CLI arguments passed directly to API

---

## 2. Trust Boundaries

```
┌─────────────────────────────────────────────────────────────────┐
│                     UNTRUSTED ZONE                              │
│  External Internet - Telegram Users, HTTP Clients, WS Clients  │
└────────────────────┬────────────────────────────────────────────┘
                     │ API Key / User Allowlist
┌─────────────────────────────────────────────────────────────────┐
│                   AUTHENTICATED ZONE                            │
│  Foci Gateway Process - HTTP Handlers, Bot, Agents, Tools      │
└────────────────────┬────────────────────────────────────────────┘
                     │ Group Dropping (CAP_SETGID)
┌─────────────────────────────────────────────────────────────────┐
│                  CHILD PROCESS ZONE                            │
│  Shell commands, Tmux sessions, MCP servers                    │
│  ❌ No access to foci-secrets group                            │
└────────────────────┬────────────────────────────────────────────┘
                     │ API Authentication
┌─────────────────────────────────────────────────────────────────┐
│                  EXTERNAL SERVICES                            │
│  Anthropic, Gemini, OpenAI, Telegram, Brave, Bitwarden        │
└─────────────────────────────────────────────────────────────────┘
```

**Boundary Analysis:**
1. **Untrusted → Authenticated:** API keys/user allowlists (MEDIUM risk - query param logging)
2. **Authenticated → Child Processes:** Group dropping (LOW risk - OS-enforced)
3. **Authenticated → External Services:** API keys in memory (HIGH risk - no encryption at rest)
4. **Tool Execution → External Resources:** Unrestricted system access (HIGH risk)

---

## 3. Data Flow Analysis

### 3.1 Credential Flow
```
secrets.toml (root:foci-secrets 0660)
    ↓ Load at startup
secrets.Store (memory)
    ↓ Resolve templates {{secret:NAME}}
Tool Execution / API Clients
    ↓ Domain validation (allowed_hosts)
External Service (API endpoint)
```

**Critical Observation:** Linear, well-controlled path from file → memory → tools/API → external.

### 3.2 User Message Flow
```
User Input (Telegram/HTTP/WebSocket)
    ↓ Validate (allowlist/API key)
Message Queue
    ↓ Transform (optional)
Agent Loop (load session → build prompt → call API → execute tools → save)
    ↓ Tool execution (redaction + size guard)
API Response → Telegram/HTTP
```

### 3.3 Session Data Flow
```
Session Store (JSONL files)
    ↓ Load on demand
Agent Memory
    ↓ Process turn
Append Messages
    ↓ Compaction (optional)
Rewrite Session (summary)
```

---

## 4. Threat Model Matrix

### 4.1 Attack Scenarios

| Scenario | Entry Point | Attack Vector | Assets at Risk | Severity | Likelihood |
|----------|-------------|---------------|----------------|----------|------------|
| Credential Theft | Shell tool | Agent tries `cat secrets.toml` | All credentials | Critical | Low |
| Credential Exfiltration | http_request | Send secrets to attacker server | All credentials | Critical | Medium |
| Command Injection | HTTP /send | Malformed user input | System execution | High | Medium |
| SSRF Attack | http_request | Access internal services | Internal network | High | Medium |
| Path Traversal | read/write tools | Access files outside workspace | File system | High | Medium |
| Session Hijacking | HTTP API | Stolen API key | Agent sessions | High | Medium |
| DoS | HTTP/Telegram | Flood messages | Availability | Medium | High |
| Privilege Escalation | Child processes | Exploit tool execution | System access | Critical | Low |

### 4.2 Attack Surface Summary

**High-Risk:**
1. Tool Execution (shell, HTTP, file ops)
2. Credential Management (high-value target)
3. HTTP API (network-exposed)
4. Session Management (conversation history)

**Medium-Risk:**
1. Telegram Integration (allowlist protection)
2. WebSocket Voice (authenticated, large payloads)
3. Session Storage (file-based)

---

## 5. Multi-Tenancy Analysis

### 5.1 Agent Isolation

**✅ Implemented:**
- Per-agent tool registries
- Per-agent sessions (key namespacing: `agent:ID:...`)
- Per-agent memory (separate indices)
- Per-agent credentials (overrides)
- Per-agent Telegram bots

**⚠️ Concerns:**
- Shared process (all agents share memory)
- Shared file system (same user)
- Shared database connections
- No agent-to-agent auth

**❌ Missing:**
- No resource quotas
- No access control between agents
- No audit logging per agent

---

## 6. Key Security Controls

### 6.1 Authentication Controls
| Control | Strength | Gap |
|---------|----------|-----|
| HTTP API Auth | Strong | Query param logging risk |
| Telegram Auth | Medium | No message signing |
| WebSocket Auth | Strong | Same as HTTP |
| Provider Auth | Strong | Tokens in memory |

### 6.2 Authorization Controls
| Control | Strength | Gap |
|---------|----------|-----|
| Tool Execution | Strong | No tool-level auth |
| File Access | Weak | Relies on OS |
| HTTP Requests | Strong | Per-secret config |
| Slash Commands | Medium | No command-level auth |

### 6.3 Secrets Protection Controls
| Control | Strength | Gap |
|---------|----------|-----|
| File Permissions | Strong | Requires setup |
| Group Dropping | Strong | Requires CAP_SETGID |
| Response Redaction | Medium | <4 chars not redacted |
| Domain Locking | Strong | Config burden |

---

## 7. Critical Findings

### Finding 1.1: API Key in Query Parameters (MEDIUM)
**Location:** `cmd/foci-gw/http.go:71-96`
**Issue:** May be logged in access logs, browser history, proxy logs
**Recommendation:** Deprecate query param auth, enforce header-only

### Finding 1.2: No Rate Limiting (MEDIUM)
**Location:** `cmd/foci-gw/http_handlers.go`
**Issue:** All HTTP endpoints lack rate limiting
**Recommendation:** Implement per-IP and per-API-key limits

### Finding 1.3: No Request Size Limits (MEDIUM)
**Location:** `cmd/foci-gw/http_handlers.go`
**Issue:** No explicit size limits before JSON decoding
**Recommendation:** Implement `http.MaxBytesReader`

### Finding 1.4: Shared Process for Multi-Tenancy (LOW)
**Location:** `cmd/foci-gw/main.go`
**Issue:** All agents share same process memory
**Recommendation:** Document security model; consider process isolation

### Finding 1.5: No Agent-to-Agent Access Control (LOW)
**Issue:** `send_to_session` allows cross-agent communication
**Recommendation:** Implement agent-level permission model

### Finding 1.6: Unlimited WebSocket Duration (LOW)
**Location:** `internal/voice/ws.go`
**Issue:** No maximum duration, only ping/pong
**Recommendation:** Max duration with re-authentication

### Finding 1.7: No Session Binding to Auth Identity (MEDIUM)
**Issue:** API key provides access to all sessions
**Recommendation:** Session-level access control or API key scoping

### Finding 1.8: Telegram User ID Trust (LOW)
**Location:** `internal/telegram/bot.go`
**Issue:** Trusts user ID without verification; account compromise = full access
**Recommendation:** Document trust assumption; consider optional 2FA

---

## 8. Threat Model Summary

### 8.1 Most Likely Attack Paths
1. Authenticated User → Tool Execution → Credential Exfiltration (Medium likelihood, Critical impact)
2. External Attacker → API Key Theft → Full System Access (Low likelihood, Critical impact)
3. Agent Prompt Injection → Malicious Tool Use (Medium likelihood, High impact)
4. DoS → Resource Exhaustion (High likelihood, Medium impact)

### 8.2 Highest Value Targets
1. secrets.toml
2. API keys in memory
3. Session files
4. Tool execution context

### 8.3 Strongest Controls
1. OS-level secrets protection (group dropping)
2. Domain locking
3. Response redaction
4. Constant-time auth comparison

### 8.4 Weakest Controls
1. No rate limiting
2. No request size limits
3. Query parameter auth
4. No multi-tenant isolation

---

## 9. Phase Prioritization

**Critical:** Phase 2 (Secrets), Phase 3 (Tools), Phase 4 (Input Validation)
**High:** Phase 5 (Network), Phase 7 (Auth)
**Medium:** Phase 8 (Process Isolation), Phase 6 (File System)

---

## 10. Open Questions

1. Any unlisted entry points (debug endpoints, admin interfaces)?
2. Exact implementation of IsBlockedCommand/IsBlockedPath?
3. How are credentials passed to child processes (if at all)?
4. What happens if group dropping fails mid-execution?
5. Race conditions in turn lock implementation?

---

**Phase 1 Status:** ✅ COMPLETE
**Next Phase:** Phase 2 - Secrets & Credential Management
