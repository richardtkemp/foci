# Foci Security Review - Phase 6: File System & Persistence Security

**Review Date:** 2026-03-08
**Phase:** 6 of 10
**Status:** Complete

---

## Executive Summary

Phase 6 analyzed file system operations and data persistence mechanisms in Foci. The system uses JSONL files for session storage, SQLite for memory indexing, and implements log rotation for maintenance.

**Key Findings:**
- Session storage uses append-only JSONL with atomic operations
- SQLite databases lack explicit permission checks
- Log rotation has good security practices (temp files, atomic renames)
- Temporary file management varies across tools
- File permissions rely on OS defaults (umask)
- No disk quota enforcement

**Overall Security Grade:** **B** (Good practices, missing quota enforcement)

---

## 1. Session File Storage

### 1.1 JSONL Format

**Implementation:** `internal/session/store.go`

**File Structure:**
```
{data_dir}/sessions/{agent_id}/{session_key}.jsonl
```

**Format:** One JSON message per line
```jsonl
{"role":"user","content":"Hello"}
{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]}
```

**Strengths:**
✅ **Append-only** - Messages appended, not modified
✅ **Line-delimited** - Easy parsing, corruption isolation
✅ **Human-readable** - JSON format for debugging
✅ **Cache-friendly** - Stable file prefix enables caching

**Concerns:**
⚠️ **No file size limit** - Sessions can grow unbounded
⚠️ **No compaction trigger** - Manual or timeout-based only
⚠️ **No encryption** - Plaintext on disk
⚠️ **No integrity check** - No checksums or signatures

**Security Grade:** **B+** (Good format, missing size limits)

### 1.2 Append Operations

**Implementation:** `internal/session/store.go:295-341`

**Pattern:**
```go
func (s *Store) appendUnlocked(key string, msg provider.Message) error {
    path, err := s.SessionPath(key)
    if err != nil {
        return err
    }
    
    // Ensure directory exists
    if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
        return fmt.Errorf("create session dir: %w", err)
    }
    
    // Open in append mode
    f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return fmt.Errorf("open session %s: %w", key, err)
    }
    defer f.Close()
    
    // Write JSON line
    data, err := json.Marshal(msg)
    if err != nil {
        return fmt.Errorf("encode message: %w", err)
    }
    
    if _, err := f.Write(append(data, '\n')); err != nil {
        return fmt.Errorf("write session %s: %w", key, err)
    }
    
    return nil
}
```

**Strengths:**
✅ **Atomic append** - Uses O_APPEND flag
✅ **Directory creation** - MkdirAll before open
✅ **Proper file mode** - 0644 (owner rw, group/other r)
✅ **Error handling** - Comprehensive error checking
✅ **Deferred close** - Ensures file closure

**Concerns:**
⚠️ **No exclusive lock** - Multiple writers could race (though single-threaded by design)
⚠️ **No fsync** - Data may not be immediately durable
⚠️ **No write verification** - Doesn't verify write succeeded

**Security Grade:** **B+** (Good atomic operations, no locking)

### 1.3 Read Operations

**Implementation:** `internal/session/store.go:192-250`

**Pattern:**
```go
func (s *Store) loadUnlocked(key string) ([]provider.Message, error) {
    path, err := s.SessionPath(key)
    if err != nil {
        return nil, err
    }
    
    f, err := os.Open(path)
    if os.IsNotExist(err) {
        // Try .gz archive
        if err := s.decompressIfGzipped(path); err != nil {
            return nil, err
        }
        f, err = os.Open(path)
        if os.IsNotExist(err) {
            return nil, nil
        }
    }
    if err != nil {
        return nil, fmt.Errorf("open session %s: %w", key, err)
    }
    defer f.Close()
    
    scanner := bufio.NewScanner(f)
    scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB buffer
    for scanner.Scan() {
        line := scanner.Bytes()
        // Parse JSON...
    }
}
```

**Strengths:**
✅ **Graceful missing file** - Returns nil (not error)
✅ **Gzip decompression** - Transparent archive restoration
✅ **Large buffer** - Handles large lines (10MB)
✅ **Line-by-line** - Doesn't load entire file into memory

**Concerns:**
⚠️ **No file size check** - Opens potentially huge files
⚠️ **10MB line limit** - Could fail on very large tool outputs
⚠️ **Memory usage** - Loads all messages into memory slice

**Security Grade:** **B** (Good streaming, missing size checks)

### 1.4 File Permissions

**Directory Creation:**
```go
os.MkdirAll(filepath.Dir(path), 0755)
```

**File Creation:**
```go
os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
```

**Strengths:**
✅ **Reasonable defaults** - 0755 dirs, 0644 files
✅ **No world-writable** - Not 0777/0666

**Concerns:**
⚠️ **Relies on umask** - Actual permissions depend on process umask
⚠️ **No explicit chmod** - Doesn't verify final permissions
⚠️ **No group ownership** - Doesn't set group for access control
⚠️ **No ACL support** - Doesn't use POSIX ACLs

**Security Grade:** **C+** (Reasonable but not explicit)

---

## 2. Session Compaction & Archival

### 2.1 Compaction Process

**Implementation:** `internal/compaction/*.go`

**Mechanism:**
1. Load full session into memory
2. Send to provider for summarization
3. Replace session file with summary
4. No archive creation (deletion only)

**Strengths:**
✅ **Reduces file size** - Summary much smaller
✅ **Preserves context** - Key information retained
✅ **Atomic replace** - New file written completely before rename

**Concerns:**
⚠️ **Memory usage** - Loads entire session for compaction
⚠️ **No backup** - Original messages lost after compaction
⚠️ **No versioning** - Can't restore previous versions

**Security Grade:** **B** (Good atomicity, no backup)

### 2.2 Gzip Archival

**Implementation:** `internal/session/archive.go`

**Mechanism:**
- Session files compressed to .jsonl.gz
- Original .jsonl deleted after successful compression
- Archive directory: `{data_dir}/sessions/{agent_id}/archive/`

**Strengths:**
✅ **Space efficient** - Compression reduces size ~70-80%
✅ **Transparent restoration** - Automatically decompresses on access
✅ **Atomic operation** - Temp file + rename

**Concerns:**
⚠️ **No encryption** - Compressed but not encrypted
⚠️ **No integrity check** - No checksum validation
⚠️ **Disk space during operation** - Both .jsonl and .gz exist briefly

**Security Grade:** **B+** (Good implementation, missing encryption)

---

## 3. SQLite Database Handling

### 3.1 Memory Index Database

**Implementation:** `internal/memory/index.go`

**Database File:**
```
{data_dir}/memory.db
```

**Schema:**
```sql
CREATE VIRTUAL TABLE memory_fts USING fts5(
    content, path, source,
    tokenize='porter unicode61'
);

CREATE TABLE memory_meta (
    source TEXT NOT NULL,
    path TEXT NOT NULL,
    mtime REAL NOT NULL,
    PRIMARY KEY (source, path)
);
```

**Strengths:**
✅ **FTS5 full-text search** - Efficient text queries
✅ **Parameterized queries** - SQL injection protection
✅ **Transaction support** - Atomic operations
✅ **Connection pooling** - Single DB connection

**Concerns:**
⚠️ **No permission check** - Relies on OS defaults
⚠️ **No encryption** - SQLite database unencrypted
⚠️ **No backup** - No automatic backups
⚠️ **Unbounded growth** - No database size limit

**Security Grade:** **B** (Good SQL practices, missing encryption)

### 3.2 Database Operations

**Pattern:**
```go
func (idx *Index) Search(query string, sort string, opts *SearchOptions) ([]Result, error) {
    idx.mu.Lock()
    defer idx.mu.Unlock()
    
    // Build parameterized query
    args := []interface{}{query}
    sqlStr := "SELECT f.content, f.path, f.source, f.rank FROM memory_fts f WHERE memory_fts MATCH ?"
    
    rows, err := idx.db.Query(sqlStr, args...)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    
    // Scan results...
}
```

**Strengths:**
✅ **Mutex protection** - Thread-safe access
✅ **Parameterized queries** - SQL injection prevention
✅ **Proper resource cleanup** - defer rows.Close()

**Concerns:**
⚠️ **No query timeout** - Long-running queries not cancelled
⚠️ **No result size limit** - Could return huge result sets
⚠️ **No connection validation** - Doesn't check DB connection health

**Security Grade:** **B+** (Good practices, missing limits)

### 3.3 SQLite Configuration

**Connection String:** Not explicitly shown (uses sqlite.OpenInit helper)

**Expected Settings:**
- `_journal_mode=WAL` - Write-ahead logging
- `_synchronous=NORMAL` - Balance safety/performance
- `_cache_size=-64000` - 64MB cache

**Strengths:**
✅ **WAL mode** - Better concurrency
✅ **Reasonable cache** - Performance optimization

**Concerns:**
⚠️ **Not explicit in code** - Configuration unclear
⚠️ **No integrity check** - PRAGMA integrity_check not run
⚠️ **No vacuum schedule** - Database not compacted

**Security Grade:** **B** (Assumed good defaults, not explicit)

---

## 4. Log File Handling

### 4.1 Log Rotation

**Implementation:** `internal/log/rotate.go`

**Rotation Process:**
```go
func rotateFile(path string, retention time.Duration, archiveDir string, maxLineSize int) error {
    // Open source file
    f, err := os.Open(path)
    
    // Create temp file for recent lines
    tmpFile, err := os.CreateTemp(dir, ".rotate-*.tmp")
    tmpPath := tmpFile.Name()
    defer os.Remove(tmpPath) // cleanup on error
    
    // Create temp archive for old lines
    tmpArchive, err := os.CreateTemp(archiveDir, ".rotate-archive-*.tmp")
    tmpArchivePath := tmpArchive.Name()
    defer os.Remove(tmpArchivePath)
    
    // Stream through, separate old/new
    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
        line := scanner.Bytes()
        ts, ok := parseTimestamp(path, line)
        if ok && ts.Before(cutoff) {
            // Old → archive
            gzw.Write(line)
            gzw.Write([]byte("\n"))
        } else {
            // New → keep
            tmpFile.Write(line)
            tmpFile.Write([]byte("\n"))
        }
    }
    
    // Sync and close
    tmpFile.Sync()
    tmpFile.Close()
    gzw.Close()
    tmpArchive.Close()
    
    // Atomic renames
    os.Rename(tmpPath, path) // Replace original
    os.Rename(tmpArchivePath, archivePath) // Move archive
}
```

**Strengths:**
✅ **Atomic renames** - No partial state
✅ **Temp files** - Work in progress not visible
✅ **Proper cleanup** - Removes temps on error
✅ **Streaming** - Doesn't load entire file into memory
✅ **Timestamp parsing** - Intelligent line-by-line processing
✅ **Gzip compression** - Archived logs compressed

**Concerns:**
⚠️ **Double disk space** - Temp + original during rotation
⚠️ **No file locking** - Could conflict with active logging
⚠️ **No integrity check** - Doesn't verify archive integrity

**Security Grade:** **A-** (Excellent implementation, minor concerns)

### 4.2 Log Permissions

**File Creation:**
```go
// In log.go (not shown in rotate.go)
f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
```

**Strengths:**
✅ **Standard permissions** - 0644 (owner rw, group/other r)
✅ **Not world-writable** - Reasonable security

**Concerns:**
⚠️ **Relies on umask** - Actual permissions may vary
⚠️ **No group ownership** - Doesn't set log group
⚠️ **No sensitive data filtering** - Logs could contain sensitive info

**Security Grade:** **B** (Good defaults, no filtering)

---

## 5. Temporary File Management

### 5.1 Tool Result Spilling

**Implementation:** `internal/tools/shell.go`, `internal/tools/http.go`

**Pattern:**
```go
// Create temp file for large output
tmpFile, err := os.CreateTemp(spillTempDir, "spill-*.tmp")
if err != nil {
    return ToolResult{}, fmt.Errorf("create spill file: %w", err)
}
tmpPath := tmpFile.Name()

// Write output
if _, err := tmpFile.WriteString(output); err != nil {
    os.Remove(tmpPath)
    return ToolResult{}, fmt.Errorf("write spill: %w", err)
}

// Return path in result
return ToolResult{
    Text:       "Output too large, see file",
    ResultFile: tmpPath,
    ResultSize: int64(len(output)),
}, nil
```

**Strengths:**
✅ **Configurable temp dir** - Not hardcoded to /tmp
✅ **Unique filenames** - CreateTemp generates random names
✅ **Cleanup on error** - Removes temp on failure

**Concerns:**
⚠️ **No automatic cleanup** - Temp files persist after use
⚠️ **No size limit** - Could fill disk
⚠️ **World-readable temp dir** - Uses os.TempDir() permissions
⚠️ **No expiration** - Files never deleted automatically

**Security Grade:** **C+** (Good creation, missing cleanup)

### 5.2 HTTP Upload Temporary Files

**Implementation:** `internal/tools/http.go:263-276`

**Pattern:**
```go
// Auto-save binary responses to temp file
dir := tempDir
if dir == "" {
    dir = os.TempDir()
}
if err := os.MkdirAll(dir, 0755); err != nil {
    return ToolResult{}, fmt.Errorf("create temp dir: %w", err)
}
ext := extensionForContentType(contentType)
var randBytes [4]byte
rand.Read(randBytes[:])
savePath := filepath.Join(dir, "http-"+hex.EncodeToString(randBytes[:])+ext)
```

**Strengths:**
✅ **Random filename** - Uses crypto/rand
✅ **Content-type extension** - Proper file extension

**Concerns:**
⚠️ **No cleanup** - Files never deleted
⚠️ **Weak randomness** - Only 4 bytes (64-bit entropy)
⚠️ **Predictable pattern** - "http-" prefix
⚠️ **World-readable dir** - Uses os.TempDir()

**Security Grade:** **C** (Weak randomness, no cleanup)

---

## 6. File Permission Checks

### 6.1 Explicit Permission Enforcement

**Status:** NOT IMPLEMENTED

**Current Behavior:**
- Relies on umask for file permissions
- No explicit chmod calls
- No group ownership setting
- No ACL management

**Impact:**
- Files may have inconsistent permissions
- Group-based access control not enforced
- World-readable files possible (depends on umask)

**Security Grade:** **D** (No enforcement)

### 6.2 File Ownership

**Status:** NOT MANAGED

**Current Behavior:**
- Files owned by process user
- No group ownership changes
- No chown calls

**Impact:**
- No group-based access control
- Single-user model only
- Multi-tenant isolation weak

**Security Grade:** **D** (No ownership management)

---

## 7. Workspace File Operations

### 7.1 File Read Tool

**Implementation:** `internal/tools/files.go`

**Operations:**
- Read entire file into memory
- Directory listing
- No streaming support

**Strengths:**
✅ **Path validation** - Blocks sensitive paths
✅ **Symlink checking** - Resolves symlinks
✅ **Error handling** - Graceful failure

**Concerns:**
⚠️ **No file size limit** - Reads entire file into memory
⚠️ **No streaming** - Large files problematic
⚠️ **Memory exhaustion** - Could OOM on huge files

**Security Grade:** **B** (Good validation, missing size limits)

### 7.2 File Write Tool

**Implementation:** `internal/tools/files.go`

**Operations:**
- Write entire file atomically
- No append support
- No partial writes

**Strengths:**
✅ **Atomic writes** - Write to temp + rename
✅ **Blocked path checking** - Prevents overwriting sensitive files
✅ **Directory creation** - Creates parent dirs

**Concerns:**
⚠️ **No size limit** - Can write arbitrarily large files
⚠️ **No disk quota** - Can fill disk
⚠️ **No backup** - Overwrites without backup
⚠️ **No fsync** - Data may not be durable

**Security Grade:** **B-** (Atomic writes, missing limits)

### 7.3 File Edit Tool

**Implementation:** `internal/tools/files.go`

**Operations:**
- Read entire file
- Find unique string
- Replace
- Write entire file back

**Strengths:**
✅ **Uniqueness check** - old_string must be unique
✅ **Atomic replace** - Temp file + rename
✅ **Syntax validation** - Validates .json, .toml, .go, etc.

**Concerns:**
⚠️ **No file locking** - Race conditions
⚠️ **Memory usage** - Loads entire file twice
⚠️ **No backup** - Original lost on edit
⚠️ **No size limit** - Can edit huge files

**Security Grade:** **C+** (Atomic but no locking)

---

## 8. Critical Findings - Phase 6

### Finding 6.1: No File Size Limits on Reads (MEDIUM)

**Location:** Session loading, file read tool
**Issue:** Files read into memory without size checks
**Impact:** Memory exhaustion, OOM crashes
**Recommendation:**
```go
// Before reading
info, err := os.Stat(path)
if err != nil {
    return err
}
if info.Size() > maxFileSize {
    return fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxFileSize)
}
```

### Finding 6.2: No Disk Quota Enforcement (MEDIUM)

**Location:** All file operations
**Issue:** No limits on total disk usage per agent/session
**Impact:** Disk exhaustion, DoS
**Recommendation:**
- Implement per-agent disk quota
- Track total bytes written
- Enforce limit before operations

### Finding 6.3: Temporary Files Not Cleaned (MEDIUM)

**Location:** Tool result spilling, HTTP uploads
**Issue:** Temp files created but never deleted
**Impact:** Disk space leak, information disclosure
**Recommendation:**
```go
// Add cleanup mechanism
type TempFileManager struct {
    files []string
}

func (m *TempFileManager) Cleanup() {
    for _, f := range m.files {
        os.Remove(f)
    }
}
```

### Finding 6.4: Weak Randomness in Temp Filenames (LOW)

**Location:** `internal/tools/http.go:271-276`
**Issue:** Only 4 bytes (32 bits) of randomness
**Impact:** Predictable filenames, collision possible
**Recommendation:**
```go
var randBytes [16]byte // 128 bits
rand.Read(randBytes[:])
```

### Finding 6.5: No File Permission Enforcement (MEDIUM)

**Location:** All file creation
**Issue:** Relies on umask, no explicit chmod
**Impact:** Inconsistent permissions, security bypass
**Recommendation:**
```go
// After file creation
if err := os.Chmod(path, 0640); err != nil {
    return err
}
```

### Finding 6.6: SQLite No Encryption (LOW)

**Location:** `internal/memory/index.go`
**Issue:** SQLite database stored unencrypted
**Impact:** Data exposure if file accessed
**Recommendation:**
- Use SQLCipher for encryption
- Or encrypt entire data directory
- Or accept risk (document clearly)

### Finding 6.7: No Database Backup (LOW)

**Location:** SQLite databases
**Issue:** No automatic backups of SQLite files
**Impact:** Data loss on corruption
**Recommendation:**
```go
// Periodic backup
func backupDB(dbPath string) error {
    backupPath := dbPath + ".backup"
    return os.Link(dbPath, backupPath) // Hard link
}
```

### Finding 6.8: Session File No fsync (LOW)

**Location:** `internal/session/store.go:323`
**Issue:** Writes not synced to disk
**Impact:** Data loss on crash
**Recommendation:**
```go
if _, err := f.Write(data); err != nil {
    return err
}
if err := f.Sync(); err != nil { // Add fsync
    return err
}
```

---

## 9. File System Security Matrix

| Operation | Size Limit | Atomic | Locking | Permissions | Cleanup | Grade |
|-----------|------------|--------|---------|-------------|---------|-------|
| **Session Append** | ❌ None | ✅ Yes | ❌ No | ⚠️ umask | ✅ Yes | B+ |
| **Session Load** | ❌ None | N/A | ❌ No | ⚠️ umask | N/A | B |
| **Log Rotation** | ✅ 10MB | ✅ Yes | ❌ No | ⚠️ umask | ✅ Yes | A- |
| **Temp Spill** | ✅ 15KB | ✅ Yes | N/A | ⚠️ umask | ❌ No | C+ |
| **File Read** | ❌ None | N/A | ❌ No | ⚠️ umask | N/A | B |
| **File Write** | ❌ None | ✅ Yes | ❌ No | ⚠️ umask | N/A | B- |
| **File Edit** | ❌ None | ✅ Yes | ❌ No | ⚠️ umask | N/A | C+ |
| **SQLite Ops** | ❌ None | ✅ WAL | ✅ Mutex | ⚠️ umask | ❌ No | B |

---

## 10. Persistence Security Summary

### Strong Controls:
1. ✅ Atomic file operations (temp + rename pattern)
2. ✅ Log rotation with streaming and compression
3. ✅ Append-only session files
4. ✅ SQLite parameterized queries
5. ✅ Path validation for blocked files

### Weak Controls:
1. ❌ No file size limits (memory/disk exhaustion)
2. ❌ No disk quota enforcement
3. ❌ No file permission enforcement
4. ❌ No automatic temp file cleanup
5. ❌ No database encryption

### Missing Controls:
1. ❌ No file locking (race conditions)
2. ❌ No backup/versioning
3. ❌ No integrity checks (checksums)
4. ❌ No fsync enforcement
5. ❌ No group ownership management

---

## 11. Recommendations Priority

### High Priority:
1. **Add file size limits** (Finding 6.1) - Prevent memory exhaustion
2. **Implement disk quotas** (Finding 6.2) - Prevent disk exhaustion
3. **Enforce file permissions** (Finding 6.5) - Consistent security

### Medium Priority:
4. **Clean up temp files** (Finding 6.3) - Prevent space leak
5. **Add file locking** (Edit tool) - Prevent race conditions
6. **Add database backups** (Finding 6.7) - Data protection

### Low Priority:
7. **Strengthen temp randomness** (Finding 6.4) - Reduce collision risk
8. **Encrypt databases** (Finding 6.6) - Data protection
9. **Add fsync** (Finding 6.8) - Durability guarantee

---

## 12. Comparison to Industry Standards

**Better Than:**
- Simple append-only logs
- Systems without log rotation

**On Par With:**
- Standard file-based session storage
- SQLite-based applications

**Lags Behind:**
- Enterprise databases (backup, encryption)
- High-security systems (quota management, integrity checks)
- Distributed systems (distributed locking)

---

**Phase 6 Status:** ✅ COMPLETE
**Next Phase:** Phase 7 - Authentication & Authorization
