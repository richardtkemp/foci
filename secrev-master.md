# Foci Security Review - Master Plan

**Project:** Foci - Multi-Agent AI Platform
**Review Start Date:** 2026-03-07
**Review Type:** Comprehensive Security Audit
**Total Phases:** 10

---

## Executive Summary

This document outlines a comprehensive, phased security review of the Foci agent platform. Foci is a Go-based AI agent system with multi-provider support (Anthropic, Gemini, OpenAI), Telegram integration, HTTP API, and a sophisticated secrets management system.

### Project Scope

- **Language:** Go
- **Architecture:** Single binary, multi-agent support, no framework dependencies
- **Key Components:**
  - Provider clients (Anthropic, Gemini, OpenAI)
  - Telegram bot integration
  - HTTP API server
  - Session management (JSONL + SQLite)
  - Tools system (shell, http, file operations, tmux, etc.)
  - Secrets management (secrets.toml + Bitwarden)
  - Memory system (FTS5/bleve search)

### Security-Critical Areas

1. **Secrets Management** - OS-level protection, domain locking, redaction
2. **Authentication** - Telegram, HTTP API, provider API keys
3. **Tool Security** - Shell exec, HTTP requests, file operations
4. **Input Validation** - Commands, parameters, session keys
5. **Network Security** - HTTP endpoints, provider clients, WebSocket
6. **Data Protection** - Session storage, databases, logs
7. **Process Isolation** - Child processes, group dropping, MCP servers

---

## Review Phases

### Phase 1: Architecture & Threat Model Analysis ✅ COMPLETE

**Objective:** Establish a comprehensive understanding of the attack surface and threat landscape.

**Scope:**
- Map all entry points (Telegram, HTTP API, WebSocket, CLI)
- Identify trust boundaries (user → agent → tools → external services)
- Document data flows for sensitive information (credentials, user messages, session data)
- Analyze the security model documentation vs implementation
- Identify multi-tenancy isolation requirements (multiple agents)

**Deliverable:** `secrev-phase1.md`

**Status:** ✅ COMPLETE - 8 findings identified

**Key Findings:**
- 6 HTTP API endpoints + Telegram + WebSocket + CLI entry points
- No rate limiting on any endpoints
- No request size limits
- Query parameter auth logging risk
- Multi-tenancy lacks resource quotas

---

### Phase 2: Secrets & Credential Management ✅ COMPLETE

**Objective:** Deep analysis of how secrets are stored, accessed, and protected.

**Scope:**
- `secrets.toml` handling (file permissions, ownership, loading)
- Bitwarden integration (two-tier approval, session management)
- Secret template resolution (`{{secret:NAME}}`)
- Domain locking implementation for `http_request`
- Response redaction for secrets in output
- Group dropping for child processes
- API key handling for all providers
- HTTP API key generation and validation

**Deliverable:** `secrev-phase2.md`

**Status:** ✅ COMPLETE - 8 findings identified

**Key Findings:**
- Excellent OS-level protection via group dropping
- Domain locking prevents credential exfiltration
- Response redaction skips values <4 chars
- API key only 52-bit entropy
- No memory protection for secrets
- Bitwarden passwords cached in plaintext

---

### Phase 3: Tool Security Analysis ✅ COMPLETE

**Objective:** Comprehensive security review of each tool's implementation.

**Scope:**
- Shell tool (command injection, process management, timeouts)
- HTTP request tool (SSRF, redirect attacks, header injection)
- File tools (path traversal, symlink attacks, permission escalation)
- Tmux tool (session isolation, resource exhaustion, command injection)
- Web tools (query injection, API key handling, response filtering)
- Spawn tool (context isolation, tool blacklist enforcement, recursion)
- Memory search (query injection, resource exhaustion)
- Telegram tool (spam potential, media handling)
- Session send tool (cross-session injection)

**Deliverable:** `secrev-phase3.md`

**Status:** ✅ COMPLETE - 8 findings identified

**Key Findings:**
- Shell: B+ grade, regex-based detection concerns
- HTTP: B grade, SSRF allows internal network access
- Files: C+ grade, no sandboxing by default
- Tmux: B grade, unlimited resource consumption
- Spawn: A- grade, excellent context isolation
- Critical: SSRF allows internal IPs, no rate limiting

---

### Phase 4: Input Validation & Injection Attacks ✅ COMPLETE

**Objective:** Identify all input vectors and validate sanitization.

**Scope:**
- HTTP API endpoint parameter validation
- Telegram message handling and parsing
- Slash command parsing and dispatch
- Tool parameter JSON parsing
- Session key parsing and validation
- File path validation in tools
- Configuration file parsing (TOML)
- MCP server input handling

**Attack Vectors Tested:**
1. **Command Injection:** Malformed slash commands, special characters
2. **Path Traversal:** `../`, absolute paths, symlinks in file paths
3. **JSON Injection:** Malformed tool parameters, type confusion
4. **Session Key Manipulation:** Invalid formats, special characters
5. **Header Injection:** HTTP headers, newline characters
6. **TOML Injection:** Malformed config, special characters in values

**Methodology:**
1. Analyzed all input parsing code
2. Checked null/empty handling
3. Verified type assertions and conversions
4. Tested boundary conditions (max sizes, empty strings, unicode)
5. Checked error messages for information disclosure

**Deliverable:** `secrev-phase4.md`

**Status:** ✅ COMPLETE - 8 findings identified

**Key Findings:**
- No request size limits on HTTP endpoints or tool parameters (HIGH)
- Tool parameter schemas exist but not enforced at runtime (MEDIUM)
- Blocked path checking uses substring matching (incomplete) (MEDIUM)
- No input sanitization for Telegram messages (MEDIUM)
- Slash commands vulnerable to injection via special characters (MEDIUM)
- No rate limiting on parsing attempts (MEDIUM)
- Session keys have format validation but no security properties (LOW)
- Query parameters used without validation (LOW)

**Overall Grade:** B (Good with gaps)

**Completion Date:** 2026-03-08

---

### Phase 5: Network & API Security 📋 PLANNED

**Objective:** Review all network-facing components for security issues.

**Scope:**
- HTTP API server (endpoints, authentication, TLS)
- WebSocket endpoint (`/voice`) - protocol security, authentication
- Telegram API integration - token handling, webhook security
- Provider API clients (Anthropic, Gemini, OpenAI) - credential handling, retry logic
- MCP server connections - stdio and HTTP
- HTTP client configuration (timeouts, connection pooling)
- TLS configuration and certificate validation

**Specific Checks:**
1. **TLS Verification:** Certificate validation, hostname verification
2. **Request Size Limits:** Max body sizes, header sizes
3. **Timeout Handling:** Connection timeouts, request timeouts
4. **Retry Logic:** Exponential backoff, max retries
5. **Rate Limiting:** API rate limit handling
6. **Connection Pooling:** Resource exhaustion, connection leaks

**Deliverable:** `secrev-phase5.md`

**Estimated Time:** 4-6 hours

---

### Phase 6: File System & Persistence Security 📋 PLANNED

**Objective:** Review file system operations and data persistence.

**Scope:**
- Session file storage (JSONL format)
- SQLite database handling (sessions, memory, API logs)
- Log file handling (rotation, archival)
- Temporary file management (tool results, uploads)
- File permission checks
- Workspace file operations

**Specific Checks:**
1. **Path Sanitization:** Absolute paths, traversal attempts
2. **Permission Checks:** File/directory permissions before operations
3. **Race Conditions:** Concurrent file access, atomic operations
4. **Size Limits:** File size validation before reading/writing
5. **Atomic Operations:** Safe file writes, renames
6. **Symlink Handling:** Symlink resolution and following
7. **Temporary Files:** Secure temp file creation, cleanup

**Deliverable:** `secrev-phase6.md`

**Estimated Time:** 4-6 hours

---

### Phase 7: Authentication & Authorization 📋 PLANNED

**Objective:** Review all authentication and authorization mechanisms.

**Scope:**
- Telegram user allowlisting and validation
- HTTP API key authentication and generation
- Provider API key management (Anthropic, Gemini, OpenAI)
- OAuth token management (Claude Code credentials)
- Bitwarden authentication flow
- MCP server authentication
- Session-based access control

**Specific Checks:**
1. **Token Validation:** Timing attacks, replay attacks
2. **User Allowlisting:** Bypass attempts, ID spoofing
3. **Key Generation:** Entropy, predictability
4. **Token Refresh:** Race conditions, expiry handling
5. **Session Binding:** Session hijacking prevention
6. **Privilege Escalation:** Agent-to-agent access control

**Deliverable:** `secrev-phase7.md`

**Estimated Time:** 4-6 hours

---

### Phase 8: Process Isolation & Privilege Management 📋 PLANNED

**Objective:** Review process isolation and privilege management.

**Scope:**
- Child process spawning (shell tool, tmux, custom commands)
- Supplementary group dropping for secrets protection
- Process timeout and termination
- MCP server process isolation
- Resource limits (memory guard, tmux memory monitor)
- Capabilities management (CAP_SETGID)

**Specific Checks:**
1. **Group Dropping:** Verify `setgroups()` removes `foci-secrets` correctly
2. **Process Isolation:** Check child processes can't access parent resources
3. **Timeout Enforcement:** Verify processes are killed on timeout
4. **Resource Limits:** Memory/CPU limit enforcement
5. **Capability Management:** CAP_SETGID usage and dropping
6. **Zombie Processes:** Check for process reaping and cleanup

**Deliverable:** `secrev-phase8.md`

**Estimated Time:** 4-6 hours

---

### Phase 9: Dependency & Supply Chain Security 📋 PLANNED

**Objective:** Review third-party dependencies for known vulnerabilities and supply chain risks.

**Scope:**
- Direct dependencies in `go.mod`
- Transitive dependencies
- Build process security
- Dependency update policy
- Vendoring vs dynamic fetching

**Specific Checks:**
1. **Known CVEs:** Check all dependencies against vulnerability databases
2. **Dependency Pinning:** Verify versions are pinned
3. **Build Process:** Check for build-time code execution
4. **Update Frequency:** Identify stale dependencies
5. **Transitive Risks:** Map full dependency tree
6. **Abandoned Packages:** Check for unmaintained dependencies

**Methodology:**
1. Run `go list -m all` to get full dependency tree
2. Check each dependency against CVE databases
3. Review dependency licenses for compliance
4. Check for dependency substitution vulnerabilities
5. Verify go.sum integrity
6. Test build reproducibility

**Tools:**
- `govulncheck` - Go vulnerability scanner
- `nancy` - Dependency vulnerability checker
- `trivy` - Container/dependency scanner

**Deliverable:** `secrev-phase9.md`

**Estimated Time:** 2-4 hours

---

### Phase 10: Configuration & Deployment Security 📋 PLANNED

**Objective:** Review configuration defaults and deployment practices.

**Scope:**
- Default configuration values (security-focused)
- Configuration file permissions
- Systemd unit file security
- Environment variable handling
- Deployment scripts and procedures
- Upgrade/migration security
- Backup and recovery procedures

**Specific Checks:**
1. **Secure Defaults:** Verify security-sensitive defaults are secure
2. **File Permissions:** Check config file permission requirements
3. **Service Isolation:** Systemd sandboxing directives
4. **Environment Handling:** Sensitive data in environment variables
5. **Upgrade Path:** Safe migration of configs and data
6. **Secret Rotation:** Procedures for rotating credentials

**Deliverable:** `secrev-phase10.md`

**Estimated Time:** 2-4 hours

---

## Execution Strategy

### Prerequisites

- Go toolchain installed
- Access to CVE databases (NVD, GitHub Advisory)
- Test environment for dynamic analysis
- Code review tools (gopls, grep, static analysis)

### Tools Required

1. **Static Analysis:**
   - `govulncheck` - Go vulnerability scanner
   - `gopls` - Go language server for code navigation
   - `staticcheck` - Go static analyzer
   - Custom grep/analysis scripts

2. **Dynamic Analysis:**
   - Test Telegram bot account
   - Test API keys (Anthropic, etc.)
   - Local test environment

3. **Documentation:**
   - Markdown editor for reports
   - Diagramming tool for architecture visualization

### Timeline Estimate

| Phase | Estimated Time | Priority | Status |
|-------|----------------|----------|--------|
| Phase 1: Architecture & Threat Model | 4-6 hours | Critical | ✅ COMPLETE |
| Phase 2: Secrets & Credentials | 6-8 hours | Critical | ✅ COMPLETE |
| Phase 3: Tool Security | 8-12 hours | Critical | ✅ COMPLETE |
| Phase 4: Input Validation | 6-8 hours | High | ⏳ IN PROGRESS |
| Phase 5: Network & API | 4-6 hours | High | 📋 PLANNED |
| Phase 6: File System | 4-6 hours | Medium | 📋 PLANNED |
| Phase 7: Authentication | 4-6 hours | High | 📋 PLANNED |
| Phase 8: Process Isolation | 4-6 hours | High | 📋 PLANNED |
| Phase 9: Dependencies | 2-4 hours | Medium | 📋 PLANNED |
| Phase 10: Configuration | 2-4 hours | Medium | 📋 PLANNED |

**Total Estimated Time:** 44-68 hours
**Completed:** ~20 hours (Phases 1-3)
**Remaining:** ~24-48 hours (Phases 4-10)

### Phase Dependencies

```
Phase 1 (Architecture) ✅
    ├── Phase 2 (Secrets) ✅ - requires threat model
    ├── Phase 3 (Tools) ✅ - requires understanding data flows
    ├── Phase 4 (Input Validation) ⏳ - requires understanding entry points
    └── Phase 5 (Network) 📋 - requires understanding external interfaces

Phase 2 (Secrets) ✅
    └── Phase 7 (Authentication) 📋 - requires understanding credential handling

Phase 3 (Tools) ✅
    ├── Phase 6 (File System) 📋 - file tools reviewed
    └── Phase 8 (Process Isolation) 📋 - shell/tmux tools reviewed

Independent Phases:
    Phase 9 (Dependencies) 📋 - can run in parallel
    Phase 10 (Configuration) 📋 - can run in parallel
```

### Deliverables

Each phase produces:
1. **Phase Report** (`secrev-phaseN.md`) containing:
   - Executive summary
   - Detailed findings
   - Severity ratings (Critical/High/Medium/Low)
   - Specific code references (file:line)
   - Remediation recommendations
   - Test cases for verification

2. **Master Report Update** - after each phase, update this document with:
   - Summary of findings
   - Cross-cutting concerns identified
   - Updated risk assessment

### Final Deliverable

After all phases complete:
1. **Consolidated Security Report** (`secrev-final.md`) containing:
   - Executive summary of all findings
   - Risk matrix with prioritized vulnerabilities
   - Remediation roadmap
   - Security architecture recommendations
   - Ongoing security monitoring recommendations

---

## Risk Classification

### Severity Levels

- **Critical:** Remote code execution, credential theft, complete system compromise
- **High:** Privilege escalation, significant data exposure, auth bypass
- **Medium:** Limited data exposure, DoS vectors, configuration weaknesses
- **Low:** Information disclosure, minor logic flaws, defense-in-depth improvements

### Risk Factors

- **Exploitability:** How easily can this be exploited?
- **Impact:** What's the worst-case scenario?
- **Scope:** How many components are affected?
- **Detectability:** How likely is exploitation to be detected?

---

## Findings Summary

### Phase 1 Findings (8 total)

| ID | Severity | Finding | Status |
|----|----------|---------|--------|
| 1.1 | MEDIUM | API Key in Query Parameters | Open |
| 1.2 | MEDIUM | No Rate Limiting on HTTP Endpoints | Open |
| 1.3 | MEDIUM | No Request Size Limits | Open |
| 1.4 | LOW | Shared Process for Multi-Tenancy | Open |
| 1.5 | LOW | No Agent-to-Agent Access Control | Open |
| 1.6 | LOW | Unlimited WebSocket Duration | Open |
| 1.7 | MEDIUM | No Session Binding to Auth Identity | Open |
| 1.8 | LOW | Telegram User ID Trust | Open |

### Phase 2 Findings (8 total)

| ID | Severity | Finding | Status |
|----|----------|---------|--------|
| 2.1 | MEDIUM | Secrets in Memory Without Protection | Open |
| 2.2 | LOW | Response Redaction Skips Short Values | Open |
| 2.3 | LOW | API Key Only 52 Bits Entropy | Open |
| 2.4 | MEDIUM | Query Parameter Auth Logging Risk | Open |
| 2.5 | MEDIUM | No Rate Limiting on Auth | Open |
| 2.6 | LOW | Group Dropping Depends on Correct Setup | Open |
| 2.7 | LOW | Bitwarden Passwords Cached in Plaintext | Open |
| 2.8 | LOW | Domain Locking Configuration Burden | Open |

### Phase 3 Findings (8 total)

| ID | Severity | Finding | Status |
|----|----------|---------|--------|
| 3.1 | HIGH | SSRF Allows Internal Network Access | Open |
| 3.2 | MEDIUM | No Rate Limiting on Tools | Open |
| 3.3 | MEDIUM | File Tools Lack Sandbox by Default | Open |
| 3.4 | MEDIUM | Command Injection via Regex Bypass | Open |
| 3.5 | MEDIUM | Tmux Unlimited Resource Consumption | Open |
| 3.6 | LOW | Exec Bridge Socket Race Condition | Open |
| 3.7 | LOW | No Request Authentication Between Tools | Open |
| 3.8 | LOW | Edit Tool Race Conditions | Open |

### Total Findings Summary

- **Total Findings:** 24
- **Critical:** 0
- **High:** 1 (SSRF internal network access)
- **Medium:** 10
- **Low:** 13

### Top Priority Findings

1. **3.1 (HIGH)** - SSRF Allows Internal Network Access
2. **1.2 (MEDIUM)** - No Rate Limiting on HTTP Endpoints
3. **2.5 (MEDIUM)** - No Rate Limiting on Auth
4. **3.2 (MEDIUM)** - No Rate Limiting on Tools
5. **3.3 (MEDIUM)** - File Tools Lack Sandbox by Default

---

## Out of Scope

The following are explicitly out of scope for this review:

1. **Physical Security:** Assuming secure physical access to the server
2. **Social Engineering:** User manipulation attacks
3. **Anthropic/Gemini/OpenAI Platform Security:** Trust the provider's security
4. **Telegram Platform Security:** Trust Telegram's infrastructure
5. **Kernel-Level Exploits:** Assuming a secure operating system
6. **Side-Channel Attacks:** Timing, cache-based attacks

---

## Success Criteria

The review is successful when:

1. ✅ All entry points have been analyzed (Phases 1-3 complete)
2. ✅ All credential handling paths have been traced (Phase 2 complete)
3. ✅ All tools have been reviewed for security issues (Phase 3 complete)
4. ⏳ All input validation has been tested (Phase 4 in progress)
5. 📋 All network communications have been reviewed
6. 📋 All file operations have been checked
7. 📋 All authentication mechanisms have been validated
8. 📋 All process isolation has been verified
9. 📋 All dependencies have been checked for CVEs
10. 📋 All configuration defaults have been reviewed
11. 📋 A prioritized remediation plan exists for all findings

**Progress:** 30% complete (3 of 10 phases)

---

## Next Steps

1. ✅ Complete Phase 4: Input Validation & Injection Attacks
2. 📋 Continue with Phase 5-8 (high priority)
3. 📋 Complete Phase 9-10 (can run in parallel)
4. 📋 Compile final consolidated security report
5. 📋 Create remediation roadmap with priorities

---

## Document Maintenance

This is a living document. Update it when:
- New phases are identified
- Scope changes occur
- Timeline estimates need adjustment
- Cross-cutting concerns are discovered
- Remediation priorities shift

---

**Last Updated:** 2026-03-08
**Review Status:** 30% Complete (3 of 10 phases)
**Next Phase:** Phase 4 - Input Validation & Injection Attacks
