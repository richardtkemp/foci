# Session File Lifecycle

How and when JSONL session files are created, archived, and swept on disk.

## File Layout

```
{sessions.dir}/
  {agentID}/{type}{id}/root.jsonl          ← root session
  {agentID}/{type}{id}/b{childTS}.jsonl    ← branch
  {agentID}/{type}{id}/i{childTS}.jsonl    ← independent spawn
```

- `type` = `c` (chat) or `i` (independent)
- `id` = chat ID (e.g. `123456789`), a name (`research`), or a timestamp

Session keys are **stable identities** — compaction and `/reset` archive files
in place; the key (and its directory) never changes.

Key → path mapping is in `store.go:SessionPath` (via `ParseSessionKey`):
- Root keys (2 segments) → `{dir}/{key}/root.jsonl`
- Child keys (3 segments, last starts with `b` or `i`) → `{dir}/{key}.jsonl`

## When New Files Are Created

### 1. Root session: first append (lazy creation)

**Trigger:** First call to `Append()` or `AppendAll()` on a session key that has no file on disk.

**Code path:** `store.go:appendUnlocked`

```
os.Stat(path) → not found
os.OpenFile(path, O_APPEND|O_CREATE|O_WRONLY)
write session_meta line → {"type":"session_meta","created_at":"..."}
write message
```

Chat session keys are deterministic: `NewChatSessionKey(agentID, chatID)` →
`agentID/c{chatID}`. No key caching or persistence is needed — the same chat
always derives the same key. `chatmeta.Resolver.SessionKeyForChat` registers
platform ownership (a `registered` chat_metadata row) on first contact so
outbound routing can find the owning platform.

### 2. Branch file: immediately on branch creation

**Trigger:** `CreateBranchWithOptions(parentKey, opts)` in `branch.go`.

```
os.OpenFile(path, O_CREATE|O_EXCL)
write branch_meta line → {"type":"branch_meta","parent_key":"...","branch_point":N}
```

Branch keys are derived from the parent root: `parentKey.Branch()` →
`{rootKey}/b{time.Now().Unix()}`. The file is created eagerly (not lazily),
in the parent's directory.

### 3. Compaction: Replace archives in place

**Trigger:** Compactor calls `writer.Replace(key, compactedMessages)`.

**Code path:** `archive.go:replaceInternal`

```
1. Read branch_meta/session_meta from the old file
2. os.Rename(path, nextArchivePath(path))   ← root.jsonl → root.{timestamp}.jsonl
3. Create path                               ← same path, same key
4. Write preserved session_meta (original creation time)
5. Write compacted messages
```

**Result:** the key is unchanged; the pre-compaction content sits next to the
live file as `root.2026-03-13T10-30-00Z.jsonl`. Nothing anywhere has to be
told about a "new key" — there isn't one.

### 4. Reset (/reset command): Store.Reset + lazy re-creation

**Trigger:** `/reset` → `Agent.ResetSession` → `Store.Reset(key)`.

```
1. os.Rename(path, nextArchivePath(path))   ← archive in place
2. NO file created yet — next Append() recreates it lazily
```

The key is unchanged. Per-session state (model/effort overrides,
`cc_resume_id`, `no_compact`, …) is cleared explicitly by
`Agent.ClearSessionState`. Reflection runs on a branch created from the
pre-reset history (`PrepareSessionEndMemory` before the archive; the branch
loader recovers the parent prefix from the archive, P2-5).

## On Service Restart

**The existing session continues appending to the same file. No new root file is created.**

On startup (`sessions_init.go:initSessions`):

1. `RepairOrphans()` — walks all `.jsonl` files, finds assistant messages ending with tool_use blocks that have no tool_result response. Appends synthetic "Tool call interrupted" tool_result messages to the **existing file**.

2. `SessionIndex.Rebuild(store)` — scans all session files on disk and rebuilds the SQLite index (including the structured `agent_id`/`chat_id`/`is_root` columns derived from each key).

3. On the first message after restart, the deterministic key derivation lands in the same session automatically — nothing to restore.

After agents are set up, `handleRestartAndFirstRun()` delivers a restart notification to each agent's default session via `deliverInjectedTurn` → `HandleMessage`, producing a proper user/assistant turn pair.

**Net effect:** The same `root.jsonl` file keeps getting appended to across restarts. The restart is visible as a normal user/assistant turn in the session history.

## Why Your Session Is All In One File

A session that has been running for hours with restarts will have
**everything in a single `root.jsonl`** because:

1. Restart does NOT rotate/create a new file — restart notifications are appended as normal turns
2. The session key is deterministic, so every restart derives the same key
3. Only compaction replaces the root file (archiving the old content in place)
4. Only `/reset` archives the file (recreated lazily on the next message)

If compaction hasn't triggered, and no one ran `/reset`, that single `root.jsonl` contains every message since the session was first created.

### If grep can't find recent content

The file is append-only JSONL — recent messages are at the **end** of the file. Check:
- `tail -n 50 /path/to/root.jsonl | jq .` to see recent messages
- The file may be very large; grep may be matching earlier content
- Messages are JSON-encoded, so content may have escaped characters

## Archive Lifecycle

```
root.jsonl (active)
  ↓ compaction or /reset
root.2026-03-13T10-30-00Z.jsonl (archived in place, same directory)
  + root.jsonl (new active, same key)
  ↓ ArchiveSweep (every 6h, for sessions idle > archive_after)
root.2026-03-13T10-30-00Z.jsonl.gz (gzipped, original deleted)
```

- `ArchiveSweep` runs every 6 hours + on startup
- Only archives sessions whose last activity is older than `sessions.archive_after`
- Never archives the current session for each registered agent+chat
- Never archives sessions with active branches
- Gzipped files are transparently decompressed on `Load()` if needed
- Archives are read by the branch loader when a branch's parent was
  compacted/reset after the branch was created (P2-5 prefix recovery)

## Coding-Agent Transcripts (ccstream)

For the `claude-code` (ccstream) backend the conversation lives in Claude
Code's own transcript, `~/.claude/projects/<cwd-slug>/<sessionID>.jsonl`, not a
foci `root.jsonl`. `Backend.SessionFilePath()` derives that path from the live
session id + workdir (the same construction `ForkSession` uses to locate a
parent), so the session-index row carries it like any native file. Consequences:

- **ArchiveSweep** gzips a CC transcript once its chat is idle beyond
  `archive_after` — subject to the same guards (never the current session per
  agent+chat, never one with an active branch). A returning user on a
  long-idle chat resumes into a fresh CC session, because a gzipped transcript
  is no longer `claude --resume`-able. The `.jsonl` naming means sibling
  transcripts in the same project dir are never swept as "archive files"
  (`gzipArchiveFiles` keys on `<stem>.`, which a distinct UUID can't match).
- **PruneOrphans** drops the index row (and its `cc_resume_id`) if the
  transcript is manually deleted. It only scans `status='active'`, so archived
  rows don't collide.
- The `opencode` backend has no transcript file (server-stored); its
  `SessionFilePath()` stays `""` and these sweeps skip it.

## Summary Table

| Event | New File? | New Key? | Old File |
|---|---|---|---|
| First message in new chat | Yes (lazy) | No — deterministic | n/a |
| Normal message | No (append) | No | n/a |
| Service restart | No (append markers) | No — deterministic | n/a |
| Compaction (Replace) | Yes (same path) | **No** | Renamed to `root.{timestamp}.jsonl` |
| /reset (Store.Reset) | Yes (lazy, on next msg) | **No** | Renamed to `root.{timestamp}.jsonl` |
| Branch creation | Yes (immediate) | Yes (child key) | n/a |
| ArchiveSweep | No | No | Gzipped to `.jsonl.gz`, original deleted |

## Point-in-Time Lookup

Archive stamps mean "archived at" — a stamped file holds the session's
history up to that moment. Two provenance tables in state.db make the
timeline queryable (`session_archives` for rotations, `cc_resume_history`
for which CC session was live when), with the filename stamps as a
state.db-independent fallback:

```
foci debug at clutch/c123 2026-07-01T12:00:00Z
foci debug at clutch 3h        # a duration ago; bare agent = default session
```

prints the JSONL file covering that moment (live or archive) and the CC
resume ID observed live then.

Installs from the pre-stable-key era are migrated automatically at startup:
version directories are flattened into in-place archives (stamped with the
superseding version's time) and state.db rows are re-keyed. See
[SESSION_KEYS.md](SESSION_KEYS.md).

## Key Source Files

- `internal/session/store.go` — Append, SessionPath, fireEvent
- `internal/session/archive.go` — Reset, replaceInternal, ArchiveSweep
- `internal/session/branch.go` — CreateBranchWithOptions, LoadFull (archive prefix recovery)
- `internal/session/key.go` — SessionKey struct, deterministic constructors
- `internal/session/startup.go` — RepairOrphans
- `internal/chatmeta/resolver.go` — key derivation + platform-ownership registration
- `cmd/foci-gw/sessions_init.go` — startup wiring, event handler, archive goroutine
