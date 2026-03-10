# Foci Security Review - Phase 2: Secrets & Credential Management

**Review Date:** 2026-03-07
**Phase:** 2 of 10
**Status:** Complete

---

## Executive Summary

Phase 2 analyzed the secrets and credential management system, which implements **defense-in-depth** security with OS-level group dropping as the primary protection mechanism.

**Key Finding:** The secrets management system is **well-designed with multiple layers of protection**, but several areas warrant attention:
- Domain locking requires per-secret configuration (burden on operators)
- Group dropping depends on correct systemd setup (CAP_SETGID capability)
- Response redaction skips values <4 chars (could miss short tokens)
- No memory scrubbing of secret values after use

---

## 1. secrets.toml File Security

### 1.1 File Permissions & Ownership

**Implementation:** `internal/secrets/secrets.go:594-619`

**Security Checks Performed:**
```go
// Check owner is root (uid 0)
// Check group is foci-secrets
// Check permissions are 0660
// Check process has foci-secrets in supplementary groups
```

**Strengths:**
✅ **Kernel-enforced protection** - OS denies access at open() level
✅ **Root ownership** - Requires privilege escalation to modify
✅ **Restricted group** - Only processes with `foci-secrets` group can read
✅ **Strict permissions** - 0660 prevents world-readable access

**Concerns:**
⚠️ **Setup dependency** - Relies on correct system administration (setup.sh)
⚠️ **No runtime monitoring** - Checks only at startup, not ongoing verification
⚠️ **SkipSecurityChecks flag** - Can disable all checks with config option

**Security Grade:** **A** (Strong, with correct setup)

### 1.2 File Loading Process

**Implementation:** `internal/secrets/secrets.go:58-135`

**Flow:**
```
os.ReadFile(path) → toml.Unmarshal → flatten sections → validate restrictions
```

**Strengths:**
✅ **No shell exposure** - Direct file read, no shell commands
✅ **TOML parsing** - Structured format with type safety
✅ **Validation** - Checks for conflicting allow/deny rules
✅ **Graceful handling** - Missing file returns empty store (no error)

**Concerns:**
⚠️ **Values in memory** - All secrets loaded into process memory indefinitely
⚠️ **No encryption at rest** - secrets.toml is plaintext (relies on OS permissions)
⚠️ **No memory locking** - Could be swapped to disk under memory pressure

**Security Grade:** **B+** (Good, but no memory protection)

---

## 2. Bitwarden Integration Security

### 2.1 Two-Tier Approval Model

**Implementation:** `internal/secrets/bitwarden/bitwarden.go`

**Tier 1: Metadata (Auto-approved)**
- Command: `sudo -u bitwarden bw list items`
- Returns: Item names, URIs, folders, usernames (NO passwords)
- Security: Allowlisted in aisudo, no Telegram approval needed

**Tier 2: Passwords (Approval-required)**
- Command: `sudo -u bitwarden bw get password <id>`
- Returns: Actual password value
- Security: Requires Telegram approval via aisudo

**Strengths:**
✅ **Principle of least privilege** - Metadata separate from passwords
✅ **Human approval gate** - Password access requires explicit approval
✅ **Dedicated user** - Runs as `bitwarden` system user, not root
✅ **Session isolation** - Bitwarden user reads own session file

**Concerns:**
⚠️ **Session file security** - Bitwarden session token stored in file
⚠️ **No password memory scrubbing** - Passwords remain in cache until TTL expires
⚠️ **Approval fatigue** - Users may auto-approve without reading

**Security Grade:** **A-** (Strong design, implementation could be improved)

### 2.2 TTL-Based Caching

**Implementation:** `internal/secrets/bitwarden/bitwarden.go:97-116`

**Mechanism:**
```
GetPassword() → check cache → if expired: fetch via aisudo → cache → return
Background cleanup goroutine removes expired values
```

**Configuration:**
- `secret_ttl` (default 30m) - How long passwords stay cached
- `cleanup_interval` (default 1m) - Background cleanup frequency

**Strengths:**
✅ **Automatic expiration** - Passwords don't remain cached indefinitely
✅ **Background cleanup** - Regular removal of expired values
✅ **Configurable TTL** - Operators can adjust security/performance tradeoff

**Concerns:**
⚠️ **Memory persistence** - Passwords in memory for TTL duration
⚠️ **No encryption in memory** - Cached values are plaintext
⚠️ **TTL clock starts at fetch** - Could extend indefinitely with frequent use

**Security Grade:** **B+** (Good, with memory security concerns)

---

## 3. Secret Template Resolution

### 3.1 Template Syntax

**Pattern:** `\{\{secret:([a-zA-Z0-9_.\-]+)\}\}`

**Examples:**
- `{{secret:custom.github_token}}` - Static secret from secrets.toml
- `{{secret:bw.UUID}}` - Bitwarden vault item

**Resolution Locations:**
1. HTTP request tool (`internal/tools/http_secrets.go`)
2. Shell tool - **BLOCKED** except for Bitwarden refs

### 3.2 Shell Tool Blocking

**Implementation:** `internal/tools/shell.go:132-143`

**Logic:**
```go
// Find all {{secret:NAME}} refs in command
refs := secrets.FindSecretRefs(cmd)

// For each ref:
if !bitwarden.IsBitwardenRef(ref) && !allSecretRefsInHTTPRequestScope(cmd) {
    return error - regular secrets not allowed in exec
}
```

**Strengths:**
✅ **Blocks regular secrets** - Prevents shell command exfiltration
✅ **Allows Bitwarden** - Approval-gated, safe for exec
✅ **Allows foci_http_request scope** - Templates resolved safely in-process
✅ **Clear error message** - Tells user to use http_request tool

**Concerns:**
⚠️ **Regex-based detection** - Could miss edge cases with encoding
⚠️ **Scope detection complexity** - Relies on command parsing

**Security Grade:** **A-** (Strong, with minor edge case concerns)

### 3.3 HTTP Request Tool Resolution

**Implementation:** `internal/tools/http_secrets.go:22-68`

**Process:**
1. Scan headers, body, form_fields for secret refs
2. Validate each secret against `allowed_hosts`
3. Resolve templates with actual values
4. Return resolved values + hasSecrets flag

**Strengths:**
✅ **Pre-validation** - Checks domain locking before resolution
✅ **In-process resolution** - No shell exposure
✅ **Comprehensive scanning** - Headers, body, form fields
✅ **Error messages** - Clear feedback when validation fails

**Concerns:**
⚠️ **Multiple scans** - Regex executed multiple times
⚠️ **No template nesting** - Can't reference secrets in secret names

**Security Grade:** **A** (Excellent)

---

## 4. Domain Locking Implementation

### 4.1 allowed_hosts Mechanism

**Implementation:** `internal/secrets/secrets.go:477-492`

**Validation:**
```go
func (s *Store) CheckHostAllowed(secretName, targetURL string) error {
    hosts := s.AllowedHosts(secretName)
    if len(hosts) == 0 {
        return error - secret has no allowed_hosts
    }
    hostname := url.Parse(targetURL).Hostname() // strips userinfo and port
    for _, allowed := range hosts {
        if strings.EqualFold(hostname, allowed) {
            return nil
        }
    }
    return error - host not in allowed list
}
```

**Strengths:**
✅ **userinfo stripping** - Prevents `https://api.example.com@evil.com` attacks
✅ **Case-insensitive** - Per RFC 4343
✅ **Port stripping** - Prevents port-based bypass
✅ **Mandatory** - Secrets without allowed_hosts cannot be used

**Concerns:**
⚠️ **Configuration burden** - Every secret needs manual host configuration
⚠️ **No wildcard support** - Must list exact hosts
⚠️ **No IDN handling** - Punycode conversion not mentioned
⚠️ **No subdomain matching** - Exact match only

**Security Grade:** **A-** (Strong, could be more user-friendly)

### 4.2 Cross-Domain Redirect Blocking

**Implementation:** `internal/tools/http.go:211-222`

**Logic:**
```go
if resolved.hasSecrets {
    originalHost := req.URL.Hostname()
    client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
        if !strings.EqualFold(req.URL.Hostname(), originalHost) {
            return error - blocked cross-domain redirect
        }
        if len(via) >= 10 {
            return error - too many redirects
        }
        return nil // allow same-host redirects
    }
}
```

**Strengths:**
✅ **Automatic enforcement** - Applied when secrets detected
✅ **Prevents credential capture** - Server can't redirect to attacker
✅ **Same-host allowed** - Legitimate redirects within domain work
✅ **Redirect limit** - Prevents redirect loops

**Concerns:**
⚠️ **Only for secrets** - Non-secret requests can redirect anywhere
⚠️ **Hostname only** - Could allow redirects to different ports on same host

**Security Grade:** **A** (Excellent)

---

## 5. Response Redaction

### 5.1 Redaction Implementation

**Implementation:** `internal/secrets/secrets.go:528-538`

**Logic:**
```go
func (s *Store) Redact(text string) string {
    // Sort by length (longest first) to avoid partial redaction
    // Skip values < 4 chars to avoid false positives
    for _, value := range sortedValues {
        if len(value) >= 4 {
            text = strings.ReplaceAll(text, value, "[REDACTED]")
        }
    }
    return text
}
```

**Strengths:**
✅ **Automatic** - Applied to all tool output
✅ **Length-based sorting** - Prevents partial matches
✅ **Defense in depth** - Catches accidental leaks

**Concerns:**
TODO possibility of fishing expeditions: agent generates a large number of random/dictionary attack strings, any which are replaced in shell results by '[redacted]' are revealed to be secrets.
⚠️ **<4 char exclusion** - Short tokens/passwords not redacted
⚠️ **Case-sensitive** - Won't catch case variations
⚠️ **Substring matching** - Could over-redact similar strings
⚠️ **Performance** - Replaces all values in every response

**Security Grade:** **B+** (Good, with edge cases)

### 5.2 Application Points

**Redaction is applied:**
1. Tool results (after execution)
2. Error messages (before return)
3. HTTP response bodies (before display)
4. Shell command output (before display)

**Security Grade:** **A-** (Comprehensive coverage)

---

## 6. Group Dropping for Child Processes

### 6.1 Process Attribute Setup

**Implementation:** `internal/tools/procattr.go:81-121`

**Mechanism:**
```go
func ChildSysProcAttr() *syscall.SysProcAttr {
    return &syscall.SysProcAttr{
        Setpgid: true, // Create new process group
        Credential: &syscall.Credential{
            Uid: primaryGID,
            Gid: primaryGID,
            Groups: filteredGroups, // All groups EXCEPT foci-secrets
        },
    }
}
```

**Process:**
1. Get primary GID from current process
2. Get supplementary groups from current process
3. Filter out `foci-secrets` group (SecurityGroupName constant)
4. Set credential with filtered groups
5. Child process spawned with Setpgid=true

**Strengths:**
✅ **Kernel-enforced** - OS denies access to secrets.toml
✅ **All child processes** - Applied to shell, tmux, custom commands
✅ **CAP_SETGID capability** - Required to call setgroups()
✅ **Process group isolation** - Children can be killed as group

**Concerns:**
⚠️ **Capability dependency** - Requires CAP_SETGID in systemd unit
⚠️ **Probe-based check** - Tries `true` command to verify capability
⚠️ **Failure mode** - Logs warning but continues if capability missing
⚠️ **Only supplementary groups** - Primary GID preserved

**Security Grade:** **A** (Excellent, with setup dependency)

### 6.2 Application Points

**Group dropping is applied to:**
1. Shell tool (exec commands) - `internal/tools/shell.go`
2. Tmux tool (tmux commands) - `internal/tools/tmux.go`
3. Custom commands (slash command scripts) - `internal/command/builtins.go`

**Verification:**
- Startup check logs warning if CAP_SETGID unavailable
- Documentation requires `AmbientCapabilities=CAP_SETGID` in systemd unit

**Security Grade:** **A** (Comprehensive)

---

## 7. API Key Security

### 7.1 HTTP API Key Generation

**Implementation:** `internal/secrets/secrets.go:22-36`

**Algorithm:**
```go
func GeneratePassphrase(wordCount int) (string, error) {
    // Use crypto/rand
    // Pick from EFF Short Wordlist (1296 words)
    // 5 words ≈ 52 bits of entropy
    // Example: "maple-thunder-basket-olive-crane"
}
```

**Strengths:**
✅ **Cryptographic RNG** - Uses crypto/rand, not math/rand
✅ **High entropy** - 52 bits (5 words × ~10.3 bits/word)
✅ **Passphrase format** - Human-readable, memorable
✅ **Auto-generated** - Created on first startup if missing

**Concerns:**
⚠️ **Only 52 bits** - Could be brute-forced with significant resources (though unlikely)
⚠️ **Word list size** - Only 1296 words, limiting entropy per word
⚠️ **No rotation** - Key never changes unless manually rotated

**Security Grade:** **B+** (Good, could be stronger)

### 7.2 HTTP API Key Validation

**Implementation:** `cmd/foci-gw/http.go:71-96`

**Mechanism:**
```go
// Check Authorization: Bearer header
// Fallback: api_key query param (for WebSocket compat)
// Use subtle.ConstantTimeCompare to prevent timing attacks
if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
    return 403 Forbidden
}
```

**Strengths:**
✅ **Timing-safe comparison** - Prevents timing attacks
✅ **Multiple auth methods** - Header or query param
✅ **Constant-time** - Uses crypto/subtle package

**Concerns:**
⚠️ **Query parameter auth** - Logged in access logs, browser history
⚠️ **No rate limiting** - Unlimited authentication attempts
⚠️ **No session binding** - API key grants access to all sessions

**Security Grade:** **B+** (Good crypto, weak transport)

### 7.3 Provider API Keys

**Storage:** In-memory in provider client structs
**Access:** Direct function calls from provider clients
**Rotation:** Manual (update secrets.toml + hot reload or restart)

**Strengths:**
✅ **Not in agent context** - Never exposed to LLM
✅ **Per-endpoint** - Separate clients for different providers

**Concerns:**
⚠️ **Memory persistence** - Keys remain in memory indefinitely
⚠️ **No encryption** - Plaintext in memory
⚠️ **No automatic rotation** - Manual process

**Security Grade:** **B** (Acceptable, could be improved)

---

## 8. Critical Findings - Phase 2

### Finding 2.1: Secrets in Memory Without Protection (MEDIUM)

**Location:** `internal/secrets/secrets.go:46-55`
**Issue:** All secret values stored in plaintext in memory map, could be swapped to disk or exposed in memory dumps
**Recommendation:** 
- Consider using `mlock()` to prevent swapping
- Zero memory after use where possible
- Consider encrypted memory regions for high-value secrets

### Finding 2.2: Response Redaction Skips Short Values (LOW)

**Location:** `internal/secrets/secrets.go:528-538`
**Issue:** Values < 4 characters not redacted, could miss short tokens/codes
**Recommendation:** 
- Lower threshold to 2-3 characters
- Or make threshold configurable
- Add special handling for known token formats

### Finding 2.3: API Key Only 52 Bits Entropy (LOW)

**Location:** `internal/secrets/secrets.go:22-36`
**Issue:** 5-word passphrase provides ~52 bits, sufficient but not ideal for long-term secrets
**Recommendation:**
- Consider 6-7 words for ~62-73 bits
- Document entropy expectations
- Support manual key rotation

### Finding 2.4: Query Parameter Auth Logging Risk (MEDIUM)

**Location:** `cmd/foci-gw/http.go:71-96`
**Issue:** API key can be passed as query parameter, may appear in logs/history
**Recommendation:**
- Deprecate query parameter auth
- Add warning header when query param used
- Log redaction for api_key parameters

### Finding 2.5: No Rate Limiting on Auth (MEDIUM)

**Location:** All HTTP endpoints
**Issue:** Unlimited authentication attempts allowed
**Recommendation:**
- Implement per-IP rate limiting
- Implement per-key rate limiting
- Add failed auth attempt logging

### Finding 2.6: Group Dropping Depends on Correct Setup (LOW)

**Location:** `internal/tools/procattr.go:81-121`
**Issue:** CAP_SETGID capability must be configured in systemd; logs warning but continues if missing
**Recommendation:**
- Make capability check fatal if secrets.toml exists
- Add setup verification command
- Document required systemd configuration clearly

### Finding 2.7: Bitwarden Passwords Cached in Plaintext (LOW)

**Location:** `internal/secrets/bitwarden/bitwarden.go:97-116`
**Issue:** Unlocked passwords cached in memory map until TTL expires
**Recommendation:**
- Consider encrypting cached values with session key
- Zero cache entries after use
- Shorter default TTL

### Finding 2.8: Domain Locking Configuration Burden (LOW)

**Location:** `internal/secrets/secrets.go:477-492`
**Issue:** Every secret requires manual `allowed_hosts` configuration, prone to errors/omissions
**Recommendation:**
- Support wildcard hosts (*.example.com)
- Auto-suggest hosts from URL patterns
- Add validation/linting for secrets.toml

---

## 9. Strengths Summary

**Excellent Security Controls:**
1. ✅ **OS-level secrets protection** - Kernel-enforced via group dropping
2. ✅ **Domain locking** - Prevents credential exfiltration
3. ✅ **Cross-domain redirect blocking** - Automatic when secrets present
4. ✅ **Shell secret blocking** - Regular secrets cannot reach child processes
5. ✅ **Two-tier Bitwarden** - Approval-gated password access
6. ✅ **Response redaction** - Defense-in-depth for leaks
7. ✅ **Constant-time auth** - Prevents timing attacks
8. ✅ **Cryptographic RNG** - Secure API key generation

**Good Security Controls:**
1. ✅ **File permission checks** - Validates secrets.toml security at startup
2. ✅ **Template resolution** - In-process, no shell exposure
3. ✅ **TTL-based caching** - Bitwarden passwords expire
4. ✅ **Process group isolation** - Children in separate groups

---

## 10. Attack Surface Analysis

### 10.1 Credential Theft Attack Paths

**Path 1: Direct File Read**
- Entry: Shell tool
- Attack: `cat secrets.toml`
- Protection: Group dropping (kernel denies access)
- **Risk: CRITICAL** → **Mitigated to LOW**

**Path 2: HTTP Exfiltration**
- Entry: http_request tool
- Attack: Send secret to attacker.com
- Protection: Domain locking (host validation)
- **Risk: CRITICAL** → **Mitigated to LOW**

**Path 3: Memory Dump**
- Entry: Process crash/core dump
- Attack: Read secrets from memory
- Protection: None (plaintext in memory)
- **Risk: HIGH** → **UNMITIGATED**

**Path 4: Timing Attack on Auth**
- Entry: HTTP API
- Attack: Measure response times to guess API key
- Protection: Constant-time comparison
- **Risk: MEDIUM** → **Mitigated to VERY LOW**

**Path 5: Bitwarden Approval Bypass**
- Entry: bitwarden_unlock tool
- Attack: Social engineering for approval
- Protection: Human approval gate
- **Risk: MEDIUM** → **Mitigated to LOW** (human factor)

### 10.2 Most Likely Attack Scenarios

1. **Misconfigured Setup** - CAP_SETGID missing, group dropping fails → **Severity: HIGH**
2. **Query Param Logging** - API key logged → **Severity: MEDIUM**
3. **Memory Forensics** - Process memory captured → **Severity: MEDIUM**
4. **Brute Force API Key** - With unlimited attempts → **Severity: LOW** (52-bit entropy)

---

## 11. Recommendations Priority

### Critical Priority:
1. **Add rate limiting** to auth endpoints (Finding 2.5)
2. **Make CAP_SETGID check fatal** when secrets exist (Finding 2.6)

### High Priority:
3. **Memory protection** for secrets (Finding 2.1)
4. **Deprecate query param auth** (Finding 2.4)

### Medium Priority:
5. **Lower redaction threshold** or make configurable (Finding 2.2)
6. **Encrypt cached Bitwarden values** (Finding 2.7)
7. **Increase API key entropy** to 6-7 words (Finding 2.3)

### Low Priority:
8. **Wildcard support** for allowed_hosts (Finding 2.8)

---

## 12. Compliance & Best Practices

**Follows Best Practices:**
✅ Defense in depth (OS + application + redaction)
✅ Principle of least privilege (group dropping)
✅ Secure by default (mandatory allowed_hosts)
✅ No secrets in logs (redaction)
✅ Human approval for sensitive operations (Bitwarden)

**Could Improve:**
⚠️ Memory protection (no mlock, no zeroing)
⚠️ Secret rotation (manual process only)
⚠️ Audit logging (limited credential access logging)

---

## 13. Testing Coverage

**Well-Tested Areas:**
✅ File permission checks (secrets_security_test.go)
✅ Path blocking (secrets_blocking_test.go)
✅ Host validation (secrets_hosts_test.go)
✅ Secret resolution (secrets_resolution_test.go)
✅ Response redaction (secrets_redaction_test.go)
✅ Bitwarden integration (bitwarden/bitwarden_test.go)

**Missing Test Coverage:**
⚠️ Group dropping verification (integration test needed)
⚠️ Memory behavior under pressure
⚠️ Concurrent secret access patterns
⚠️ Secret rotation scenarios

---

## 14. Comparison to Industry Standards

**Better Than:**
- Most AI agent platforms (no secrets in context)
- Applications with hardcoded credentials
- Systems without domain locking

**On Par With:**
- Enterprise secrets management
- Production-grade credential handling

**Could Adopt From:**
- HashiCorp Vault (dynamic secrets, automatic rotation)
- AWS Secrets Manager (encrypted at rest, audit logging)
- SPIFFE/SPIRE (workload identity, short-lived credentials)

---

## 15. Questions for Next Phase

1. How are credentials passed to provider API clients during initialization?
2. What happens to in-flight API calls when credentials are hot-reloaded?
3. Are there any race conditions in credential access from multiple goroutines?
4. How does the system behave if secrets.toml is modified at runtime?
5. What logging exists for credential access and usage?

---

**Phase 2 Status:** ✅ COMPLETE
**Next Phase:** Phase 3 - Tool Security Analysis
