# Foci Security Review - Phase 4: Input Validation & Injection Attacks

**Review Date:** 2026-03-08
**Phase:** 4 of 10
**Status:** Complete

---

## Executive Summary

Phase 4 analyzed input validation mechanisms across all attack vectors in the Foci system. Input validation is the first line of defense against injection attacks, data corruption, and unexpected behavior.

**Key Findings:**
- JSON parsing uses standard library (encoding/json) - robust but doesn't validate schema
- Path validation has symlink protection but no sandbox enforcement
- Command parsing uses simple string splitting - vulnerable to injection via special characters
- HTTP parameters lack size limits and strict type checking
- Session keys have format validation but no security properties
- TOML parsing is robust (BurntSushi/toml library)

**Overall Security Grade:** **B** (Good with gaps)

---

## 1. HTTP API Parameter Validation

### 1.1 JSON Request Body Parsing

**Locations:** `cmd/foci-gw/http_handlers.go`

**Pattern:**
```go
var req struct {
    Agent      string `json:"agent"`
    Session    string `json:"session"`
    Text       string `json:"text"`
    IfActive   string `json:"if_active"`
    IfInactive string `json:"if_inactive"`
    Async      bool   `json:"async"`
}
if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
    http.Error(w, "bad request: need {\"text\": \"...\"}", http.StatusBadRequest)
    return
}
```

**Validation Mechanisms:**
- JSON syntax validation (built-in)
- Required field check (req.Text == "")
- Type checking via struct tags

**Strengths:**
✅ Standard library (encoding/json) - well-tested
✅ Type safety via struct tags
✅ Required field validation

**Concerns:**
⚠️ **No size limit on request body** - Could accept multi-GB payloads
⚠️ **No field length limits** - Strings could be arbitrarily long
⚠️ **No character validation** - Could contain control characters, null bytes
⚠️ **Error messages expose internal structure** - "need {\"text\": \"...\"}"
⚠️ **No rate limiting** - Unlimited parsing attempts

**Attack Vectors:**
1. **Memory exhaustion** - Send 1GB JSON payload
2. **Deep nesting** - JSON with 1000 levels of nesting
3. **Unicode attacks** - Invalid UTF-8 sequences
4. **Null byte injection** - Strings containing `\x00`
5. **Type confusion** - Send array instead of string

**Testing:**
```bash
# Memory exhaustion
curl -X POST http://localhost:18791/send \
  -H "Authorization: Bearer key" \
  -d "$(python3 -c 'print("{\"text\": \"" + "A"*1000000000 + "\"}")')"

# Deep nesting
curl -X POST http://localhost:18791/send \
  -H "Authorization: Bearer key" \
  -d '{"text": "test", "nested": {"a": {"a": {"a": ...}}}}' # 1000 levels
```

**Security Grade:** **C+** (Missing size limits, no rate limiting)

### 1.2 Query Parameter Validation

**Locations:** `cmd/foci-gw/http_handlers.go:127`

**Pattern:**
```go
agentID := r.URL.Query().Get("agent")
```

**Validation Mechanisms:**
- None - raw query parameter extraction

**Strengths:**
✅ None (no validation)

**Concerns:**
⚠️ **No sanitization** - Direct use of user input
⚠️ **No length limit** - Could be arbitrarily long
⚠️ **No character validation** - Could contain special characters
⚠️ **Reflected in error messages** - Exposed in "unknown agent: %q"

**Attack Vectors:**
1. **Log injection** - Send query param with newlines
2. **Error message XSS** - If errors rendered in HTML
3. **Buffer overflow** - Extremely long query parameters

**Security Grade:** **D** (No validation)

---

## 2. Tool Parameter JSON Parsing

### 2.1 Parameter Structure

**Locations:** All tools in `internal/tools/*.go`

**Pattern:**
```go
var p struct {
    Command string `json:"command"`
    Timeout int    `json:"timeout"`
    Background bool `json:"background"`
    OutputMode string `json:"output_mode"`
}
if err := json.Unmarshal(params, &p); err != nil {
    return ToolResult{}, fmt.Errorf("parse params: %w", err)
}
```

**Validation Mechanisms:**
- JSON unmarshal with type checking
- No additional validation after unmarshal

**Strengths:**
✅ Type safety via struct unmarshal
✅ Clear error messages for malformed JSON
✅ Consistent pattern across all tools

**Concerns:**
⚠️ **No field validation** - Accepts any value after type check
⚠️ **No length limits** - String fields can be arbitrarily long
⚠️ **No range checks** - Integers can be any value (including negative)
⚠️ **No enum validation** - Strings not checked against valid values

**Examples of Weak Validation:**

**Shell Tool:**
```go
// No validation on command length
// No validation on timeout range (could be negative or huge)
// No validation on output_mode (should be "combined" or "separated")
```

**HTTP Request Tool:**
```go
// No validation on URL format beyond basic parsing
// No validation on header values (could contain newlines)
// No validation on method (should be GET/POST/PUT/PATCH/DELETE/HEAD)
```

**File Tools:**
```go
// Path validation exists but no length limit
// No validation on content length
// No validation on old_string/new_string uniqueness before search
```

**Attack Vectors:**
1. **Integer overflow** - Timeout = -1 or MaxInt64
2. **String length attacks** - Command with 1GB string
3. **Enum injection** - OutputMode = "../../../etc/passwd"
4. **Type coercion** - Send string where int expected (caught by JSON)

**Security Grade:** **C** (Type safety only, no semantic validation)

### 2.2 Tool Parameter Schema

**Location:** Tool registration in `internal/tools/registry.go`

**Pattern:**
```go
Parameters: json.RawMessage(`{
    "type": "object",
    "properties": {
        "command": {
            "type": "string",
            "description": "Shell command to execute."
        }
    },
    "required": ["command"]
}`)
```

**Validation Mechanisms:**
- Schema exists for documentation
- **NOT ENFORCED** at runtime

**Strengths:**
✅ Schema documentation exists
✅ Clear property definitions

**Concerns:**
⚠️ **Schema not validated** - Only used for documentation/API
⚠️ **No runtime enforcement** - Types checked by JSON unmarshal, not schema
⚠️ **No additional constraints** - MinLength, MaxLength, Pattern not enforced

**Security Grade:** **D** (Schema exists but not enforced)

---

## 3. Slash Command Parsing

### 3.1 Command Structure

**Location:** `internal/command/command.go:143-168`

**Pattern:**
```go
func (r *Registry) Dispatch(ctx context.Context, text string) (string, bool) {
    text = strings.TrimSpace(text)
    if !strings.HasPrefix(text, "/") {
        return "", false
    }
    
    // Parse "/command args"
    text = text[1:] // strip leading /
    name, args, _ := strings.Cut(text, " ")
    name = strings.ToLower(name)
    args = strings.TrimSpace(args)
    
    cmd := r.commands[name]
    if cmd == nil {
        return r.suggestCommand(name), true
    }
    
    result, err := cmd.Execute(ctx, args)
    if err != nil {
        return "Error: " + err.Error(), true
    }
    return result, true
}
```

**Validation Mechanisms:**
- Leading slash check
- Case-insensitive command name
- String trimming

**Strengths:**
✅ Simple parsing logic
✅ Case normalization
✅ Clear error messages
✅ Command suggestions for typos

**Concerns:**
⚠️ **No escaping** - Special characters in args passed directly
⚠️ **No length limit** - Command or args could be very long
⚠️ **No character validation** - Could contain control characters
⚠️ **Args unvalidated** - Passed directly to command handler

**Attack Vectors:**
1. **Command injection** - `/cmd ; rm -rf /`
2. **Newline injection** - `/cmd arg1\narg2`
3. **Null byte injection** - `/cmd arg\x00hidden`
4. **Unicode normalization** - Different Unicode representations
5. **Long command DoS** - `/cmd [1MB string]`

**Testing:**
```bash
# Command injection attempt
/cmd test; rm -rf /

# Newline injection
/cmd "arg1
arg2"

# Null byte
/cmd "test\x00hidden"
```

**Security Grade:** **C** (Basic parsing, no sanitization)

### 3.2 Command Argument Handling

**Pattern:** Command handlers receive raw `args string`

**Example (`/secrets` command):**
```go
Execute: func(ctx context.Context, args string) (string, error) {
    // args is unparsed string
    parts := strings.Fields(args) // Simple splitting
    // ...
}
```

**Validation Mechanisms:**
- None in dispatcher
- Per-command validation varies

**Concerns:**
⚠️ **Inconsistent validation** - Each command validates differently
⚠️ **No standard sanitization** - Raw strings passed to handlers
⚠️ **Quote handling varies** - Some commands parse quotes, others don't

**Security Grade:** **C-** (Inconsistent, per-command validation)

---

## 4. File Path Validation

### 4.1 Path Sanitization

**Location:** `internal/tools/files.go:161-210`

**Pattern:**
```go
func resolveAndValidatePath(path, baseDir string) (string, error) {
    if baseDir == "" {
        return path, nil // No validation!
    }
    
    evalBase, err := filepath.EvalSymlinks(baseDir)
    if err != nil {
        return "", fmt.Errorf("resolve base dir: %w", err)
    }
    
    if filepath.IsAbs(path) {
        return "", fmt.Errorf("absolute paths not allowed in isolated mode")
    }
    
    resolved := filepath.Clean(filepath.Join(baseDir, path))
    
    evalResolved, err := filepath.EvalSymlinks(resolved)
    if err != nil && !os.IsNotExist(err) {
        return "", fmt.Errorf("resolve path: %w", err)
    }
    if err == nil {
        resolved = evalResolved
    } else {
        // File doesn't exist — resolve deepest existing ancestor
        // to catch symlinks on intermediate components
        // [complex symlink checking logic]
    }
    
    // Check resolved path is under baseDir
    if !strings.HasPrefix(resolved, evalBase) {
        return "", fmt.Errorf("path escapes base directory")
    }
    
    return resolved, nil
}
```

**Validation Mechanisms:**
- Symlink resolution (filepath.EvalSymlinks)
- Path cleaning (filepath.Clean)
- Absolute path blocking (in isolated mode)
- Traversal prevention (prefix check)
- TOCTOU mitigation (resolve ancestors for non-existent files)

**Strengths:**
✅ **Excellent symlink protection** - Resolves all symlinks
✅ **TOCTOU awareness** - Handles non-existent file edge case
✅ **Path cleaning** - Normalizes path separators
✅ **Containment check** - Verifies path stays in baseDir

**Concerns:**
⚠️ **No isolation by default** - baseDir="" allows full filesystem access
⚠️ **Race condition still possible** - Symlink could change between check and use
⚠️ **No length limit** - Paths could be very long
⚠️ **No character validation** - Could contain null bytes
⚠️ **Blocked paths check is separate** - Not integrated into path resolution

**Attack Vectors:**
1. **Symlink race** - Create/change symlink after validation
2. **Long path DoS** - Path with 10000+ characters
3. **Null byte injection** - `file.txt\x00.hidden`
4. **Unicode normalization** - Different representations of same path
5. **Full access** - Use tool without isolation enabled

**Testing:**
```bash
# Symlink race
ln -s /safe/file.txt /tmp/link.txt
# Tool validates link
rm /tmp/link.txt && ln -s /etc/passwd /tmp/link.txt
# Tool uses changed link

# Long path
read path: "$(python3 -c 'print("A"*10000)')"

# Null byte (if language allows)
read path: "file.txt\x00.hidden"
```

**Security Grade:** **B+** (Excellent symlink protection, no sandbox by default)

### 4.2 Blocked Path Checking

**Location:** `internal/secrets/secrets.go:550-569`

**Pattern:**
```go
func (s *Store) IsBlockedPath(path string) bool {
    for _, blocked := range s.blockedPaths {
        if strings.Contains(path, blocked) {
            return true
        }
    }
    return false
}

func (s *Store) IsBlockedCommand(cmd string) bool {
    for _, blocked := range s.blockedPaths {
        if strings.Contains(cmd, blocked) {
            return true
        }
    }
    return false
}
```

**Blocked Paths:**
- secrets.toml
- /proc/self/environ
- .env
- credentials.json
- .aws/credentials
- .ssh/id_rsa

**Validation Mechanisms:**
- Substring matching (strings.Contains)

**Strengths:**
✅ Defense in depth (protects known sensitive files)
✅ Simple implementation
✅ Extensible (can add more paths)

**Concerns:**
⚠️ **Substring matching** - Could have false positives
⚠️ **Incomplete list** - Only covers known sensitive paths
⚠️ **Easy to bypass** - Use `/proc/./self/environ` or `.//env`
⚠️ **No normalization** - Doesn't resolve path before checking

**Attack Vectors:**
1. **Path obfuscation** - `./secrets.toml`, `secrets.toml.bak`
2. **Alternate paths** - `/proc/self/fd/3` instead of `/proc/self/environ`
3. **Encoding** - Base64 filename, URL encoding
4. **Symlink** - Link to blocked path with innocent name

**Testing:**
```bash
# Obfuscation
read path: "./secrets.toml"
read path: "secrets.toml.bak"

# Alternate proc path
read path: "/proc/self/fd/3"

# Symlink bypass
ln -s /path/to/secrets.toml /tmp/innocent.txt
read path: "/tmp/innocent.txt"
```

**Security Grade:** **C** (Incomplete, substring matching)

---

## 5. Session Key Validation

### 5.1 Key Format

**Location:** `internal/session/key.go`

**Pattern:**
```go
// Session keys have format: agent:ID:type:IDENTIFIER
// Examples:
//   agent:main:main
//   agent:main:chat:123456789
//   agent:main:spawn:spawn-1234567890
```

**Validation Mechanisms:**
- Format parsing via string splitting
- Component validation (agent ID must exist, type must be known)

**Strengths:**
✅ Structured format
✅ Namespacing (agent: prefix)
✅ Type validation

**Concerns:**
⚠️ **No length limit** - Could be very long
⚠️ **No character validation** - Could contain special characters
⚠️ **No security properties** - No secret, no signature
⚠️ **Predictable format** - Could be guessed by attacker

**Attack Vectors:**
1. **Key prediction** - Guess other session keys
2. **Key collision** - Craft key to collide with another session
3. **Long key DoS** - Extremely long session key
4. **Special characters** - Key with newlines, nulls

**Testing:**
```bash
# Key prediction
send_to_session session: "agent:other:main"

# Long key
send_to_session session: "agent:main:main$(python3 -c 'print(":A"*10000)')"

# Special chars
send_to_session session: "agent:main:main\ninjected"
```

**Security Grade:** **C** (Format validation only, no security properties)

---

## 6. Configuration Parsing (TOML)

### 6.1 TOML Parsing

**Location:** `internal/config/config.go`

**Library:** `github.com/BurntSushi/toml`

**Pattern:**
```go
var cfg Config
if _, err := toml.DecodeFile(path, &cfg); err != nil {
    return fmt.Errorf("load config: %w", err)
}
```

**Validation Mechanisms:**
- TOML syntax validation (library)
- Type checking via struct tags
- Post-parse validation (some fields)

**Strengths:**
✅ Well-tested library
✅ Type safety
✅ Clear error messages
✅ Structured format

**Concerns:**
⚠️ **No schema enforcement** - Extra fields silently ignored
⚠️ **No value validation** - Values not checked for sanity
⚠️ **No size limits** - Config could be huge
⚠️ **Error messages may leak paths** - "load config: open /path/to/config"

**Attack Vectors:**
1. **Huge config** - 1GB TOML file
2. **Deep nesting** - Deeply nested tables
3. **Type coercion** - Try to pass string where int expected
4. **Path traversal in config** - Include paths like `../../../etc/passwd`

**Testing:**
```toml
# Huge config
[agent]
name = "test"
# ... repeat 1M times ...

# Type coercion
[http]
port = "not-a-number"  # Should fail

# Path in config
[workspace]
path = "../../../etc/passwd"
```

**Security Grade:** **B+** (Robust library, weak value validation)

---

## 7. Telegram Message Handling

### 7.1 Message Parsing

**Location:** `internal/telegram/bot.go`

**Pattern:**
```go
// Message received from Telegram API
msg := update.Message
text := msg.Text

// Direct pass-through to agent
// No validation, sanitization, or filtering
```

**Validation Mechanisms:**
- None - raw message text used

**Strengths:**
✅ Preserves user intent
✅ No false positives from aggressive filtering

**Concerns:**
⚠️ **No length limit** - Could send 100KB message
⚠️ **No character filtering** - Could contain any Unicode
⚠️ **No injection protection** - Could contain markdown, HTML-like syntax
⚠️ **No sanitization** - Direct pass-through

**Attack Vectors:**
1. **Long message DoS** - Send 1MB message
2. **Markdown injection** - Telegram markdown could be interpreted
3. **Unicode attacks** - RTL override, zero-width characters
4. **Control characters** - Messages with binary data

**Testing:**
```python
# Send via Telegram API
message = "A" * 1000000  # 1MB
message = "\u202E" + "normal text"  # RTL override
message = "test\u0000hidden"  # Null byte
```

**Security Grade:** **D** (No validation)

---

## 8. MCP Server Input Handling

### 8.1 JSON-RPC Message Parsing

**Location:** `internal/mcp/*.go`

**Pattern:**
```go
// JSON-RPC messages parsed via encoding/json
var req Request
if err := json.Unmarshal(data, &req); err != nil {
    return err
}
```

**Validation Mechanisms:**
- JSON syntax validation
- JSON-RPC structure validation

**Strengths:**
✅ Standard JSON parsing
✅ JSON-RPC spec compliance

**Concerns:**
⚠️ **No size limits** - Could receive huge messages
⚠️ **No method validation** - Method names not validated
⚠️ **No parameter validation** - Params passed as-is
⚠️ **External server trust** - Trusts MCP server responses

**Security Grade:** **C+** (Standard JSON parsing, no semantic validation)

---

## 9. Critical Findings - Phase 4

### Finding 4.1: No Request Size Limits (HIGH)

**Location:** All HTTP endpoints, tool parameter parsing
**Issue:** No limits on request body size, JSON payload size, or field lengths
**Impact:** Memory exhaustion, DoS
**Recommendation:**
```go
// Add to HTTP handlers
r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024) // 10MB

// Add length validation in tools
if len(p.Command) > 10000 {
    return ToolResult{}, fmt.Errorf("command too long (max 10KB)")
}
```

### Finding 4.2: No Input Sanitization (MEDIUM)

**Location:** Command parsing, Telegram messages
**Issue:** Raw strings passed without sanitization
**Impact:** Injection attacks, log injection, display issues
**Recommendation:**
- Add character filtering (control characters, nulls)
- Add length limits
- Validate against expected patterns

### Finding 4.3: Tool Parameter Schema Not Enforced (MEDIUM)

**Location:** Tool parameter parsing
**Issue:** JSON schema exists but not validated at runtime
**Impact:** Invalid parameters accepted, unexpected behavior
**Recommendation:**
- Implement schema validation
- Use JSON Schema validator library
- Or add manual validation per tool

### Finding 4.4: Blocked Path Incomplete (MEDIUM)

**Location:** `internal/secrets/secrets.go:550-569`
**Issue:** Substring matching, incomplete list, easy to bypass
**Impact:** Access to sensitive files
**Recommendation:**
- Resolve paths before checking
- Use path prefix matching
- Expand blocked list
- Add pattern matching

### Finding 4.5: No Session Key Security Properties (LOW)

**Location:** Session key handling
**Issue:** Predictable format, no secret, no signature
**Impact:** Session key prediction, unauthorized access
**Recommendation:**
- Add random component to keys
- Add HMAC signature
- Or use UUIDs

### Finding 4.6: Slash Command Injection (MEDIUM)

**Location:** Command parsing in `internal/command/command.go`
**Issue:** Special characters in arguments not sanitized
**Impact:** Command injection, unexpected behavior
**Recommendation:**
- Add argument escaping
- Validate against injection patterns
- Use structured argument parsing

### Finding 4.7: No Rate Limiting on Parsing (MEDIUM)

**Location:** All input parsing
**Issue:** Unlimited parsing attempts allowed
**Impact:** CPU exhaustion, DoS
**Recommendation:**
- Implement rate limiting per session
- Limit parse attempts per second
- Add exponential backoff

### Finding 4.8: Query Parameter No Validation (LOW)

**Location:** HTTP query parameter handling
**Issue:** Raw query parameters used without validation
**Impact:** Log injection, error message XSS
**Recommendation:**
- Validate query parameter length
- Filter special characters
- Escape in error messages

---

## 10. Input Validation Matrix

| Input Vector | Size Limit | Type Check | Sanitization | Schema | Overall |
|--------------|------------|------------|--------------|--------|---------|
| **HTTP JSON** | ❌ None | ✅ Yes | ❌ None | ❌ No | C+ |
| **HTTP Query** | ❌ None | ❌ No | ❌ None | ❌ No | D |
| **Tool Params** | ❌ None | ✅ Yes | ❌ None | ❌ No* | C |
| **Slash Cmds** | ❌ None | ✅ Yes | ❌ None | ❌ No | C |
| **File Paths** | ❌ None | ✅ Yes | ⚠️ Partial | ❌ No | B+ |
| **Session Keys** | ❌ None | ✅ Yes | ❌ None | ⚠️ Format | C |
| **TOML Config** | ❌ None | ✅ Yes | ❌ None | ⚠️ Partial | B+ |
| **Telegram** | ❌ None | ✅ Yes | ❌ None | ❌ No | D |
| **MCP** | ❌ None | ✅ Yes | ❌ None | ⚠️ JSON-RPC | C+ |

*Schema exists but not enforced at runtime

---

## 11. Attack Surface Summary

### Highest Risk Vectors:
1. **HTTP JSON bodies** - No size limit, memory exhaustion
2. **Telegram messages** - No validation at all
3. **Tool parameters** - Schema exists but not enforced

### Most Robust Vectors:
1. **File paths** - Excellent symlink protection (with isolation)
2. **TOML config** - Well-tested library
3. **Session keys** - Format validation

---

## 12. Recommendations Priority

### Critical Priority:
1. **Add request size limits** (Finding 4.1)
2. **Implement rate limiting** (Finding 4.7)

### High Priority:
3. **Add input sanitization** (Finding 4.2)
4. **Enforce tool parameter schemas** (Finding 4.3)
5. **Improve blocked path checking** (Finding 4.4)

### Medium Priority:
6. **Add slash command escaping** (Finding 4.6)
7. **Add session key security** (Finding 4.5)
8. **Validate query parameters** (Finding 4.8)

---

## 13. Testing Coverage

**Missing Test Coverage:**
⚠️ Fuzzing of all input vectors
⚠️ Size limit testing
⚠️ Injection attack testing
⚠️ Unicode edge cases
⚠️ Control character handling
⚠️ Rate limiting verification

---

## 14. Comparison to Industry Standards

**Better Than:**
- Systems with no validation at all

**On Par With:**
- Basic web applications
- Simple API servers

**Could Adopt From:**
- OWASP Input Validation Cheat Sheet
- JSON Schema validation (ajv, gojsonschema)
-zap (Go security scanner)

---

**Phase 4 Status:** ✅ COMPLETE
**Next Phase:** Phase 5 - Network & API Security
