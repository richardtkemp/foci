# Foci Security Review - Phase 3: Tool Security Analysis

**Review Date:** 2026-03-08
**Phase:** 3 of 10
**Status:** Complete

---

## Executive Summary

Phase 3 analyzed the security of all tools in the Foci agent system. The tools provide significant system access (shell execution, HTTP requests, file operations, tmux management) with **generally strong security controls** but several areas require attention.

**Key Findings:**
- **Shell tool** has strong secret blocking and group dropping, but regex-based command parsing could miss edge cases
- **HTTP request tool** has excellent SSRF defenses (domain locking, redirect blocking), but no rate limiting
- **File tools** have path validation but rely on OS permissions without sandboxing
- **Tmux tool** has session isolation but unlimited resource consumption potential
- **Spawn tool** has comprehensive context isolation with explicit allowlists

**Overall Security Grade:** **B+** (Good with areas for improvement)

---

## 1. Shell (exec) Tool Security Analysis

### 1.1 Command Injection Prevention

**Implementation:** `internal/tools/shell.go:110-173`

**Security Controls:**
```go
// Blocked paths check
if store != nil && store.IsBlockedCommand(p.Command) {
    return error - command references blocked path
}

// Secret template blocking
if refs := secrets.FindSecretRefs(cmd); refs != nil {
    for _, ref := range refs {
        if !bitwarden.IsBitwardenRef(ref) && !allSecretRefsInHTTPRequestScope(cmd) {
            return error - secrets not allowed in exec
        }
    }
}
```

**Strengths:**
✅ **Blocked path checking** - Prevents access to secrets.toml, /proc/self/environ
✅ **Secret template blocking** - Regular secrets cannot reach child processes
✅ **Bitwarden exception** - Approval-gated secrets allowed (safe via aisudo)
✅ **foci_http_request scope detection** - Templates allowed in tool arguments
✅ **Group dropping** - Child processes lose foci-secrets group access
✅ **Process group isolation** - Children in separate process groups

**Concerns:**
⚠️ **Regex-based detection** - `FindSecretRefs` uses regex, could miss encoded variants
⚠️ **Scope detection complexity** - `allSecretRefsInHTTPRequestScope` parses command structure
⚠️ **Encoding attacks** - Could use base64/hex encoding to bypass detection
⚠️ **Command separator parsing** - Relies on regex for pipe/semicolon detection

**Attack Vectors:**
1. **Encoded commands** - `echo c2VjcmV0 | base64 -d | sh` (secret in base64)
2. **Environment variable injection** - Passing secrets via env vars
3. **File descriptor passing** - Passing secrets via /dev/fd/N
4. **Symlink attacks** - Creating symlinks to blocked paths with innocent names

**Mitigation:**
- Group dropping prevents access to secrets.toml (kernel-enforced)
- But secrets could be exfiltrated via other mechanisms (env, encoding)

**Security Grade:** **B+** (Good, with edge case concerns)

### 1.2 Process Management

**Implementation:** `internal/tools/shell.go:175-239`

**Security Controls:**
```go
// Timeout enforcement
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()

// Process termination on timeout
proc.Cancel = func() error {
    return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
}

// Output size limits
stdoutSpill, stderrSpill, combinedSpill, doneRead := startPipeReaders(
    stdout, stderr, outputMode, spillThreshold, spillTempDir)
```

**Strengths:**
✅ **Timeout enforcement** - Default 30s, max 300s
✅ **Process group kill** - Kills entire process group on timeout
✅ **Output size limits** - Spills to disk after 15KB (default)
✅ **Memory protection** - Uses io.LimitReader to cap memory
✅ **Background mode** - Separate execution path for long-running commands

**Concerns:**
⚠️ **Zombie processes** - Background processes could become zombies if not reaped
⚠️ **Resource exhaustion** - Multiple concurrent long-running commands
⚠️ **Auto-background timing** - Commands background after threshold, continue running

**Security Grade:** **A-** (Excellent, minor concerns)

### 1.3 Exec Bridge (Tool Piping)

**Implementation:** `internal/tools/execbridge.go`

**Mechanism:**
```
exec subprocess ←→ Unix socket ←→ foci process
                     ↓
              foci-call binary
                     ↓
              Tool execution
```

**Security Controls:**
- Unix socket with 0600 permissions
- Per-shell-connection socket (unique path)
- Socket cleanup on command exit
- JSON message format

**Strengths:**
✅ **Unix socket isolation** - File permissions restrict access
✅ **Per-connection socket** - No shared state between execs
✅ **Automatic cleanup** - Socket and funcs file removed after command
✅ **No network exposure** - Unix domain socket only

**Concerns:**
⚠️ **Socket path predictability** - `/tmp/foci-exec-<pid>-<n>.sock`
⚠️ **Race condition** - Socket exists briefly before permission set
⚠️ **No authentication** - Any process with access can connect

**Security Grade:** **B+** (Good, with minor race condition)

---

## 2. HTTP Request Tool Security Analysis

### 2.1 SSRF (Server-Side Request Forgery) Prevention

**Implementation:** `internal/tools/http_secrets.go:96-123`

**Security Controls:**
```go
// Domain locking for regular secrets
for _, name := range regularRefs {
    if err := store.CheckHostAllowed(name, reqURL); err != nil {
        return error - host not allowed
    }
}

// Domain locking for Bitwarden secrets
for _, name := range bwRefs {
    if err := bwStore.CheckHostAllowed(id, reqURL); err != nil {
        return error - host not allowed
    }
}
```

**Strengths:**
✅ **Mandatory domain locking** - Secrets without allowed_hosts rejected
✅ **Pre-validation** - Checks BEFORE sending request
✅ **userinfo stripping** - Prevents `https://api.example.com@evil.com` attacks
✅ **Port stripping** - Prevents port-based bypass
✅ **Case-insensitive** - Prevents case-based bypass

**Concerns:**
⚠️ **No IP address blocking** - Could access internal IPs (192.168.x.x, 10.x.x.x)
⚠️ **DNS rebinding** - DNS could change after validation
⚠️ **IPv6 scope** - IPv6 addresses not explicitly handled
⚠️ **No localhost blocking** - Could access 127.0.0.1, localhost

**Attack Vectors:**
1. **Internal network access** - `http://192.168.1.1/admin`
2. **Local service access** - `http://localhost:8080/metrics`
3. **Cloud metadata** - `http://169.254.169.254/latest/meta-data/`
4. **DNS rebinding** - Attacker controls DNS, changes after validation

**Mitigation:**
- Domain locking prevents credential exfiltration
- But attacker could still access internal services (without credentials)

**Security Grade:** **B** (Good for credentials, weak for general SSRF)

### 2.2 Redirect Attack Prevention

**Implementation:** `internal/tools/http.go:211-222`

**Security Controls:**
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
        return nil
    }
}
```

**Strengths:**
✅ **Automatic enforcement** - Applied when secrets detected
✅ **Cross-domain blocking** - Prevents credential capture via redirect
✅ **Same-host allowed** - Legitimate same-domain redirects work
✅ **Redirect limit** - Max 10 redirects

**Concerns:**
⚠️ **Only with secrets** - Non-secret requests can redirect anywhere
⚠️ **Hostname only** - Different ports on same host allowed
⚠️ **No protocol enforcement** - HTTPS → HTTP downgrade possible (without secrets)

**Security Grade:** **A-** (Excellent, limited scope)

### 2.3 Request Size & Resource Limits

**Implementation:** `internal/tools/http.go:342-364`

**Limits:**
- Default timeout: 30s (max 300s)
- Default response: 1MB (10MB for binary/save_to)
- Max upload file: 50MB (configurable)
- Max response bytes: configurable

**Strengths:**
✅ **Timeout limits** - Prevents hanging requests
✅ **Response size limits** - Prevents memory exhaustion
✅ **Upload size limits** - Prevents disk exhaustion
✅ **Configurable** - Operators can adjust limits

**Concerns:**
⚠️ **No rate limiting** - Unlimited requests per session
⚠️ **No concurrent request limits** - Could make many parallel requests
⚠️ **Large uploads** - 50MB default could be abused

**Security Grade:** **B+** (Good, missing rate limiting)

---

## 3. File Tools Security Analysis

### 3.1 Path Traversal Prevention

**Implementation:** `internal/tools/files.go:161-210`

**Security Controls:**
```go
func resolveAndValidatePath(path, baseDir string) (string, error) {
    if baseDir == "" {
        return path, nil // No isolation - full filesystem access
    }
    
    // Block absolute paths in isolated mode
    if filepath.IsAbs(path) {
        return "", error - absolute paths not allowed
    }
    
    // Resolve and verify within baseDir
    resolved := filepath.Clean(filepath.Join(baseDir, path))
    evalResolved, err := filepath.EvalSymlinks(resolved)
    // ... symlink checking ...
}
```

**Strengths:**
✅ **Symlink resolution** - EvalSymlinks prevents symlink escapes
✅ **Path cleaning** - Normalizes path (removes ./ and ../)
✅ **Absolute path blocking** - In isolated mode only
✅ **Blocked path checking** - Prevents access to secrets.toml, .env, etc.

**Concerns:**
⚠️ **No isolation by default** - baseDir="" allows full filesystem access
⚠️ **TOCTOU race** - Symlink check then use gap
⚠️ **Incomplete blocked paths** - Only blocks known sensitive paths
⚠️ **No sandbox** - Relies on OS permissions only

**Attack Vectors:**
1. **Symlink race** - Create symlink after check but before use
2. **Unknown sensitive files** - Access files not in blocked list
3. **Full filesystem access** - Non-isolated agents can read anything

**Blocked Paths:**
- secrets.toml (config file itself)
- /proc/self/environ (environment variables)
- .env (environment files)
- credentials.json (credential files)
- .aws/credentials (AWS credentials)
- .ssh/id_rsa (SSH private keys)

**Security Grade:** **C+** (Weak without isolation, good with isolation)

### 3.2 Write Tool Security

**Implementation:** `internal/tools/files.go:212-298`

**Security Controls:**
```go
// Path validation
resolvedPath, err := resolveAndValidatePath(p.Path, baseDir, blockedPaths)
if err != nil {
    return error
}

// Blocked path checking
for _, blocked := range blockedPaths {
    if strings.Contains(resolvedPath, blocked) {
        return error - path is blocked
    }
}
```

**Strengths:**
✅ **Path validation** - Same as read tool
✅ **Blocked path checking** - Cannot overwrite sensitive files
✅ **Directory creation** - Creates parent directories safely

**Concerns:**
⚠️ **No atomic writes** - Could write partial content on crash
⚠️ **No backup** - Overwrites without backup
⚠️ **No size limits** - Can write unlimited data
⚠️ **Disk exhaustion** - Could fill disk with large files

**Security Grade:** **C+** (Same as read tool)

### 3.3 Edit Tool Security

**Implementation:** `internal/tools/files.go:300-416`

**Mechanism:**
1. Read entire file into memory
2. Find unique occurrence of old_string
3. Replace with new_string
4. Write entire file back
5. Syntax validation for certain file types

**Strengths:**
✅ **Uniqueness requirement** - old_string must be unique
✅ **Syntax validation** - Validates .json, .toml, .go, .yaml, .xml, .py, .sh
✅ **Atomic replacement** - Single string replacement

**Concerns:**
⚠️ **No locking** - Race conditions with concurrent edits
⚠️ **Memory usage** - Loads entire file into memory
⚠️ **No backup** - No undo mechanism
⚠️ **Large files** - Could exhaust memory on large files

**Security Grade:** **B** (Good, with race condition concerns)

---

## 4. Tmux Tool Security Analysis

### 4.1 Session Isolation

**Implementation:** `internal/tools/tmux.go:49-97`

**Security Controls:**
```go
type tmuxInstance struct {
    mu                sync.Mutex
    watched           map[string]*watchedSession
    owned             map[string]string // tmux session → agent session
    notifier          *AsyncNotifier
    stateStore        *state.Store
    sessionTTL        time.Duration // auto-kill idle sessions
    // ...
}
```

**Strengths:**
✅ **Per-instance state** - Each agent has isolated tmux tracking
✅ **Ownership tracking** - Knows which agent owns which tmux session
✅ **Session persistence** - State survives restarts
✅ **TTL-based cleanup** - Auto-kills idle sessions

**Concerns:**
⚠️ **No resource limits** - Unlimited tmux sessions per agent
⚠️ **No memory limits** - Tmux sessions can consume unlimited memory
⚠️ **No CPU limits** - Processes in tmux can use unlimited CPU
⚠️ **Shared tmux server** - All agents share same tmux server

**Security Grade:** **B** (Good isolation, no resource limits)

### 4.2 Command Execution in Tmux

**Implementation:** `internal/tools/tmux_ops.go`

**Security Controls:**
- Commands executed via `tmux send-keys`
- Group dropping applies (loses foci-secrets group)
- Process group isolation

**Concerns:**
⚠️ **Command injection in send** - Keys sent as literal keystrokes
⚠️ **No sanitization** - Special characters sent directly
⚠️ **Terminal control sequences** - Could send escape sequences
⚠️ **Shell metacharacters** - Commands interpreted by shell in tmux

**Attack Vectors:**
1. **Escape sequence injection** - Send `\x1b` sequences
2. **Shell command chaining** - `; rm -rf /`
3. **Terminal buffer overflow** - Send extremely long lines
4. **Special key sequences** - Ctrl+C, Ctrl+Z, etc.

**Security Grade:** **B-** (Good isolation, command injection concerns)

### 4.3 Resource Exhaustion

**Concerns:**
⚠️ **Unlimited sessions** - No limit on number of tmux sessions
⚠️ **Unlimited windows** - No limit on windows per session
⚠️ **Unlimited processes** - No limit on processes per session
⚠️ **Memory monitor** - Separate monitor exists but kills reactively, not proactively

**Mitigation:**
- TTL-based reaper kills idle sessions
- Memory monitor kills high-RSS sessions
- But both are reactive, not preventive

**Security Grade:** **C+** (Reactive, not proactive)

---

## 5. Web Tools Security Analysis

### 5.1 web_search Tool

**Implementation:** `internal/tools/web.go:28-93`

**Security Controls:**
- Query passed to search provider (Anthropic or Brave)
- Response size limits
- Result filtering

**Concerns:**
⚠️ **No query sanitization** - Queries sent directly to API
⚠️ **API key in memory** - Brave API key stored in memory
⚠️ **No rate limiting** - Unlimited search queries
⚠️ **Result content** - No sanitization of search results

**Security Grade:** **B+** (Good, relies on provider security)

### 5.2 web_fetch Tool

**Implementation:** `internal/tools/web.go:95-189`

**Security Controls:**
- URL validation
- Response size limits (1MB default)
- Content-Type filtering
- Auto-summarization for large responses

**Concerns:**
⚠️ **SSRF same as http_request** - Can fetch internal URLs
⚠️ **No domain allowlist** - Can fetch any URL
⚠️ **JavaScript execution** - If provider executes JS (unlikely)
⚠️ **Content filtering** - No sanitization of fetched content

**Security Grade:** **B** (Same SSRF concerns as http_request)

---

## 6. Spawn Tool Security Analysis

### 6.1 Context Isolation Modes

**Implementation:** `internal/tools/spawn.go`

**Modes:**
1. **raw** - No system context, isolated tool set
2. **character** - System context (character files), full tool set
3. **clone** - Branch session, full tool access, async
4. **explore** - Read-only tools only (ls, find, grep, read, memory_search, web_search, web_fetch)

**Security Controls:**
```go
// Raw mode blacklist
var spawnRawBlacklist = map[string]bool{
    "shell":           true,
    "tmux":            true,
    "send_telegram":   true,
    "send_to_session": true,
    "scratchpad":      true,
    "todo":            true,
}

// Explore mode allowlist
var spawnExploreAllowed = map[string]bool{
    "read":          true,
    "memory_search": true,
    "web_search":    true,
    "web_fetch":     true,
}
```

**Strengths:**
✅ **Explicit allowlist** - Explore mode uses allowlist (secure by default)
✅ **Blacklist for raw** - Excludes dangerous tools in raw mode
✅ **Isolated temp directories** - Each spawn gets unique temp dir
✅ **Timeout enforcement** - All modes have timeout (default 120s)
✅ **Max concurrent limit** - Semaphore limits concurrent clone spawns
✅ **Recursion prevention** - Spawn excluded from one-shot tool sets

**Concerns:**
⚠️ **Blacklist maintenance** - New tools must be added to blacklist
⚠️ **Clone mode full access** - Clone inherits all parent tools
⚠️ **No resource quotas** - Clone sessions can consume unlimited resources

**Security Grade:** **A-** (Excellent, allowlist-based explore mode)

### 6.2 Clone Mode (Branch Sessions)

**Implementation:** `internal/tools/spawn.go:221-423`

**Security Controls:**
- Semaphore limits concurrent clones (default 3)
- Async execution with result delivery
- Session namespacing (agent:ID:spawn:spawn-TIMESTAMP)
- Full tool access (same as parent)

**Concerns:**
⚠️ **Full tool inheritance** - Clones can do anything parent can
⚠️ **Resource consumption** - Multiple clones running concurrently
⚠️ **No priority system** - Clones compete with parent for resources
⚠️ **Async result delivery** - Results delivered later, could be unexpected

**Security Grade:** **B+** (Good controls, full access by design)

---

## 7. Memory Search Tool

**Implementation:** `internal/tools/memory.go`

**Security Controls:**
- FTS5 full-text search (SQLite)
- Query sanitization (basic)
- Result limits

**Concerns:**
⚠️ **SQL injection** - Query passed to FTS5 (though parameterized)
⚠️ **Memory content exposure** - Searches all stored memories
⚠️ **No access control** - All memories searchable

**Security Grade:** **A-** (Good, SQL injection mitigated by FTS5)

---

## 8. Telegram Tool

**Implementation:** `internal/tools/telegram.go`

**Security Controls:**
- Bot token stored securely
- User allowlist enforcement
- Rate limiting (implicit via Telegram API)

**Concerns:**
⚠️ **Spam potential** - Can send unlimited messages
⚠️ **Media handling** - Can send files, images, audio
⚠️ **User enumeration** - Error messages could reveal valid users

**Security Grade:** **B+** (Good, spam potential)

---

## 9. Send to Session Tool

**Implementation:** `internal/tools/session_send.go`

**Security Controls:**
- Session key validation
- Message injection into target session
- Reply routing control

**Concerns:**
⚠️ **Cross-session injection** - Can inject into any session
⚠️ **No session-level auth** - Any agent can send to any session
⚠️ **Message spoofing** - Could impersonate other users

**Security Grade:** **C+** (Weak access control)

---

## 10. Critical Findings - Phase 3

### Finding 3.1: SSRF Allows Internal Network Access (HIGH)

**Location:** `internal/tools/http.go`, `internal/tools/http_secrets.go`
**Issue:** No blocking of internal IP addresses (192.168.x.x, 10.x.x.x, 127.0.0.1, 169.254.169.254)
**Impact:** Agent could access internal services, cloud metadata
**Recommendation:**
- Block RFC 1918 private addresses
- Block localhost/loopback
- Block link-local addresses (169.254.x.x)
- Block cloud metadata endpoints

### Finding 3.2: No Rate Limiting on Tools (MEDIUM)

**Location:** All tools
**Issue:** No rate limiting on tool invocations per session
**Impact:** DoS via resource exhaustion
**Recommendation:**
- Implement per-tool rate limits
- Implement per-session tool invocation limits
- Add cost tracking per session

### Finding 3.3: File Tools Lack Sandbox by Default (MEDIUM)

**Location:** `internal/tools/files.go`
**Issue:** baseDir="" allows full filesystem access, relies only on OS permissions
**Impact:** Agent can read/write any accessible file
**Recommendation:**
- Enable isolation by default
- Implement proper sandboxing (chroot, containers, or strict baseDir enforcement)
- Add filesystem quota per session

### Finding 3.4: Command Injection via Regex Bypass (MEDIUM)

**Location:** `internal/tools/shell.go:132-143`
**Issue:** Regex-based secret detection could miss encoded variants
**Impact:** Secrets could be exfiltrated via encoding
**Recommendation:**
- Improve secret detection for encoded variants
- Consider allowlist-based command parsing
- Add heuristics for suspicious patterns

### Finding 3.5: Tmux Unlimited Resource Consumption (MEDIUM)

**Location:** `internal/tools/tmux.go`
**Issue:** No limits on tmux sessions, windows, or processes
**Impact:** Resource exhaustion (memory, CPU, processes)
**Recommendation:**
- Limit max sessions per agent
- Limit max windows per session
- Implement proactive resource monitoring

### Finding 3.6: Exec Bridge Socket Race Condition (LOW)

**Location:** `internal/tools/execbridge.go`
**Issue:** Brief window between socket creation and permission setting
**Impact:** Local attacker could connect before permissions set
**Recommendation:**
- Use umask before socket creation
- Or create in protected directory

### Finding 3.7: No Request Authentication Between Tools (LOW)

**Location:** Tool execution framework
**Issue:** Tools accept requests from any agent without authentication
**Impact:** Compromised tool could affect other agents
**Recommendation:**
- Add tool invocation authentication
- Session context binding for tool calls

### Finding 3.8: Edit Tool Race Conditions (LOW)

**Location:** `internal/tools/files.go:300-416`
**Issue:** No file locking, concurrent edits could corrupt files
**Impact:** Data corruption, race conditions
**Recommendation:**
- Implement file locking (flock)
- Add optimistic concurrency control
- Or serialize edits per file

---

## 11. Tool Security Matrix

| Tool | Injection Risk | SSRF Risk | Resource Risk | Isolation | Overall |
|------|----------------|-----------|---------------|-----------|---------|
| **shell** | MEDIUM | LOW | MEDIUM | A | B+ |
| **http_request** | LOW | HIGH | MEDIUM | A | B |
| **read** | LOW | N/A | LOW | C | C+ |
| **write** | LOW | N/A | MEDIUM | C | C+ |
| **edit** | LOW | N/A | MEDIUM | B | B |
| **tmux** | MEDIUM | N/A | HIGH | B | B- |
| **web_search** | LOW | N/A | LOW | A | B+ |
| **web_fetch** | LOW | HIGH | LOW | A | B |
| **spawn (explore)** | LOW | LOW | LOW | A | A- |
| **spawn (clone)** | LOW | MEDIUM | MEDIUM | B+ | B+ |
| **memory_search** | LOW | N/A | LOW | A | A- |
| **send_telegram** | LOW | N/A | MEDIUM | B+ | B+ |
| **send_to_session** | LOW | N/A | LOW | C | C+ |

---

## 12. Attack Surface Summary

### Highest Risk Tools:
1. **http_request** - SSRF to internal services (no credential exfiltration due to domain locking)
2. **tmux** - Resource exhaustion, command injection
3. **shell** - Command injection via encoding (mitigated by group dropping)
4. **write/edit** - Filesystem access without sandboxing

### Most Secure Tools:
1. **spawn (explore)** - Allowlist-based, read-only, isolated
2. **memory_search** - Parameterized queries, limited scope
3. **web_search** - Delegated to provider, response limits

---

## 13. Recommendations Priority

### Critical Priority:
1. **Block internal IPs** in http_request/web_fetch (Finding 3.1)
2. **Add rate limiting** to all tools (Finding 3.2)

### High Priority:
3. **Enable filesystem isolation** by default (Finding 3.3)
4. **Add resource limits** to tmux (Finding 3.5)

### Medium Priority:
5. **Improve secret detection** for encoded variants (Finding 3.4)
6. **Add file locking** to edit tool (Finding 3.8)

### Low Priority:
7. **Fix exec bridge race** condition (Finding 3.6)
8. **Add tool authentication** (Finding 3.7)

---

## 14. Testing Coverage

**Well-Tested:**
✅ Shell tool (shell_test.go - extensive)
✅ HTTP tool (http_*.go - multiple test files)
✅ Tmux tool (tmux_*.go - comprehensive test suite)
✅ Spawn tool (spawn_*.go - context isolation tested)
✅ File tools (files_test.go)

**Missing Coverage:**
⚠️ SSRF internal IP blocking (no tests)
⚠️ Rate limiting (not implemented)
⚠️ Resource exhaustion scenarios
⚠️ Race condition testing

---

## 15. Comparison to Industry Standards

**Better Than:**
- Most AI agent frameworks (actual tool isolation)
- Systems without secret protection
- Platforms with full filesystem access

**On Par With:**
- Production-grade tool systems
- Enterprise agent frameworks

**Could Adopt From:**
- OpenAI GPT Actions (OAuth, domain allowlisting)
- LangChain (tool validation frameworks)
- AutoGPT (resource constraints)

---

**Phase 3 Status:** ✅ COMPLETE
**Next Phase:** Phase 4 - Input Validation & Injection Attacks
