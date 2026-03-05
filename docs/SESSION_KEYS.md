# Session Keys

## Overview

Session keys uniquely identify conversation sessions in foci. The new format uses a hierarchical structure with version directories to handle compaction while maintaining stable identifiers for external sources like Telegram chats.

## Format

```
{agentID}/{type}{id}/{versionTS}[/{childType}{childTS}][.{n}]
```

### Components

| Component | Description | Example |
|-----------|-------------|---------|
| `agentID` | Agent identifier from config | `main` |
| `type` | Session type: `c` (chat) or `i` (independent) | `c` |
| `id` | Type-specific identifier | `123` (chat ID) or `1709596800` (timestamp) |
| `versionTS` | Version timestamp (creation or compaction time, unix seconds) | `1709590000` |
| `childType` | Child type: `b` (branch) or `i` (independent spawn) | `b` |
| `childTS` | Child timestamp (unix seconds) | `1709596800` |
| `n` | Collision counter for same-second children | `1`, `2`, `3`... |

## Session Types

### Chat Sessions (`c`)

Persistent conversation sessions with external stable IDs (e.g., Telegram chat IDs).

**Format:** `{agentID}/c{chatID}/{versionTS}`

**Examples:**
- `main/c5970082313/1709590000` - Chat session for Telegram chat ID 5970082313
- `main/c5970082313/1709600000` - Same chat after compaction (new version)

### Independent Sessions (`i`)

Ephemeral sessions without external IDs (HTTP requests, standalone tasks).

**Format:** `{agentID}/i{creationTS}/{versionTS}`

**Examples:**
- `main/i1709596800/1709596800` - Independent session (ID and initial version are the same)
- `main/i1709596800/1709600000` - Same session after compaction

## Child Sessions

All derived sessions (branches, spawns) are children of a parent session.

### Branch (`b`)

Inherits parent's message history. Used for:
- Clone spawns
- Multiball (forked interactive sessions)
- Session-end memory
- Cron/keepalive tasks

**Format:** `{parentKey}/b{timestamp}`

**Examples:**
- `main/c123/1709590000/b1709596800` - Branch from chat
- `main/i1709596800/1709596800/b1709596900` - Branch from independent
- `main/c123/1709590000/b1709596800/b1709597000` - Branch from branch

### Independent Spawn (`i`)

Spawned by parent but has separate message history. Used for:
- Non-clone spawns
- Character spawns

**Format:** `{parentKey}/i{timestamp}`

**Examples:**
- `main/c123/1709590000/i1709596801` - Independent spawn from chat
- `main/c123/1709590000/i1709596801.1` - Second spawn same second (collision)

## File Paths

Session keys map to file paths with a special rule for root sessions.

### Root Sessions

**Key:** `main/c123/1709590000`
**Path:** `sessions/main/c123/1709590000/root.jsonl`

The `/root.jsonl` suffix is added when converting key to path.

### Child Sessions

**Key:** `main/c123/1709590000/b1709596800`
**Path:** `sessions/main/c123/1709590000/b1709596800.jsonl`

The key is the path (minus extension).

### Mapping Function

```go
func SessionPath(key string) string {
    parts := strings.Split(key, "/")
    lastSegment := parts[len(parts)-1]

    // Strip collision suffix if present
    if idx := strings.Index(lastSegment, "."); idx > 0 {
        lastSegment = lastSegment[:idx]
    }

    // If last segment is pure number (version timestamp), it's a root
    if isNumeric(lastSegment) {
        return filepath.Join(sessionsDir, key, "root.jsonl")
    }

    // Otherwise it's a child
    return filepath.Join(sessionsDir, key + ".jsonl")
}
```

## Versioning and Compaction

When a session is compacted:

1. Old file is rotated: `root.jsonl` → `root.2026-03-04T02-30-00Z.jsonl`
2. New version created with new timestamp: `1709590000/` → `1709600000/`
3. Children remain in their original version directories
4. New children use the current (latest) version

### Example

**Before compaction:**
```
sessions/main/c123/
  1709590000/
    root.jsonl            ← original chat
    b1709596800.jsonl     ← branch from original
```

**After compaction:**
```
sessions/main/c123/
  1709590000/
    root.2026-03-04T02-30-00Z.jsonl  ← archived original
    b1709596800.jsonl                 ← branch still here
  1709600000/
    root.jsonl            ← new compacted chat
```

### Finding Current Version

On startup, find the latest version directory for each chat/independent session:

```go
// Scan directories under main/c123/
// Select highest numeric directory name
// Cache: currentVersion["c123"] = "1709600000"
```

For incoming messages, use the cached current version.

## API

### Constructors

```go
// Create new chat session
key := session.NewChatSessionKey("main", chatID)

// Create new independent session
key := session.IndependentSessionKey("main")

// Create branch from existing session
branchKey, err := session.BranchFromSession(parentKey)

// Create independent spawn from existing session
spawnKey, err := session.IndependentSpawnFromSession(parentKey)
```

### Parsing

```go
// Parse string key
key, err := session.ParseSessionKey("main/c123/1709590000/b1709596800")

// Access components
chatID := key.ChatID()        // 123 (or 0 if not a chat)
isRoot := key.IsRoot()        // false (has child suffix)
rootKey := key.RootKey()      // strips child suffix
```

### Manipulation

```go
// Create branch
child := key.Branch()

// Create independent spawn
spawn := key.IndependentSpawn()

// Handle collision
key2 := key.WithCollision(1)

// Change version (after compaction)
newKey := key.WithVersion(newTimestamp)
```
