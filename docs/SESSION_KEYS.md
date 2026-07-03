# Session Keys

## Overview

Session keys uniquely identify conversation sessions in foci. A key is a
**stable identity**: it never changes for the life of a conversation.
Compaction and `/reset` archive the session file *in place* — they do not mint
a new key. Anything that holds a session key (reminders, chat metadata, tmux
ownership, cron jobs, the Android app's local database) can hold it forever.

## Format

```
{agentID}/{type}{id}[/{childType}{childTS}]
```

### Components

| Component | Description | Example |
|-----------|-------------|---------|
| `agentID` | Agent identifier from config | `main` |
| `type` | Session type: `c` (chat) or `i` (independent) | `c` |
| `id` | Type-specific identifier | `123` (chat ID), `research` (name), or `1709596800` (timestamp) |
| `childType` | Child type: `b` (branch) or `i` (independent spawn) | `b` |
| `childTS` | Child creation timestamp (unix seconds) | `1709596800` |

`session.ParseSessionKey` is the **single parser** for this grammar. Nothing
outside `internal/session` should dissect key strings by hand — use
`ParseSessionKey`, `AgentIDFromKey`, `ChatIDFromKey`, or the `SessionKey`
struct methods.

## Session Types

### Chat Sessions (`c`)

Conversation sessions bound to an external chat: a Telegram chat, Discord
channel, or app conversation (whose conversationId is FNV-hashed to a chat ID).

**Format:** `{agentID}/c{chatID}` — **deterministic**: the same
`(agent, chatID)` always yields the same key (`session.NewChatSessionKey`).
No persistence is needed to reconstruct it; platforms record only *ownership*
(which platform owns the chat) via a `registered` row in `chat_metadata`.

**Example:** `main/c5970082313`

### Independent Sessions (`i`)

Sessions without an external chat binding.

- **Named** (HTTP `/send -s <name>`, adopted app conversations):
  `{agentID}/i{name}` — deterministic per name
  (`session.NamedIndependentSessionKey`). Example: `main/iresearch`
- **Anonymous** (one-off background tasks): `{agentID}/i{descriptive-id}` —
  built via the `SessionKey` struct with a caller-chosen ID (e.g.
  `main/ireflection-1709596800`).

## Child Sessions

Derived sessions are children of a **root** session. A child key has exactly
three segments; deriving a child from a child yields a *sibling* under the same
root (the true parent is recorded in the branch file's metadata, not the key).

### Branch (`b`)

Inherits parent history up to the branch point. Used for facets, clone spawns,
session-end reflection, cron/keepalive tasks.

**Format:** `{rootKey}/b{timestamp}`
**Example:** `main/c123/b1709596800`

### Independent Spawn (`i`)

Spawned by a parent but with separate history (non-clone spawns).

**Format:** `{rootKey}/i{timestamp}`

## File Paths

`Store.SessionPath` maps keys to files:

| Kind | Key | Path |
|------|-----|------|
| Root | `main/c123` | `sessions/main/c123/root.jsonl` |
| Child | `main/c123/b1709596800` | `sessions/main/c123/b1709596800.jsonl` |

A root session's directory holds its live `root.jsonl`, its archives, and its
child session files.

## Compaction and Reset (in-place archives)

There is no key rotation. Both operations archive the live file next to itself
and keep the key:

- **Compaction** (`SessionWriter.Replace`): `root.jsonl` →
  `root.<timestamp>.jsonl`, then the compacted messages are written to a fresh
  `root.jsonl`. Fires a `SessionStatusCompacted` event carrying the archive
  path.
- **Reset** (`Store.Reset`): `root.jsonl` → `root.<timestamp>.jsonl`; the next
  `Append` recreates the file lazily. Fires `SessionStatusReset`. Per-session
  state (model/effort overrides, `cc_resume_id`, `no_compact`, …) is cleared
  explicitly by `Agent.ClearSessionState` — a reset session keeps its identity
  but starts from a clean slate.

**Example directory after a compaction and a reset:**

```
sessions/main/c123/
  root.jsonl                        ← live session
  root.2026-03-04T02-30-00Z.jsonl   ← pre-compaction archive
  root.2026-05-01T09-00-00Z.jsonl   ← pre-reset archive
  b1709596800.jsonl                 ← branch (facet/reflection/…)
```

### Branches survive parent archives

Branch files start with a `{"type":"branch_meta",...}` line holding
`parent_key` and `branch_point`. `LoadFull()` reads
`parent[:branch_point] + branch's own messages`. When the parent was compacted
or reset after the branch was created, the pre-archive prefix is recovered from
the parent's newest archive file (P2-5) — this is what lets `/reset` archive a
session while its reflection branch still sees the full history.

## API

### Constructors

```go
key := session.NewChatSessionKey("main", chatID)          // "main/c123" (deterministic)
key, err := session.NamedIndependentSessionKey("main", "research") // "main/iresearch"
branchKey, err := store.CreateBranchWithOptions(parentKey, opts)   // "main/c123/b<now>"
```

### Parsing

```go
sk, err := session.ParseSessionKey("main/c123/b1709596800")
sk.AgentID   // "main"
sk.ChatID()  // 123 (0 if not a chat session)
sk.IsRoot()  // false
sk.Root()    // SessionKey for "main/c123"
```

### Point-in-time history

Keys are stable, so "where is the transcript for moment T?" is answered by
provenance, not by the key:

- **Archives** carry an "archived at" stamp — the file holds history *up to*
  that moment. Rotations are recorded in the `session_archives` table
  (`SessionIndex.RecordArchive`, written by the store event handler), and the
  filename stamps themselves are a state.db-independent fallback
  (`Store.ArchiveFileAt`).
- **CC resume IDs** are recorded in `cc_resume_history` every time a
  delegated backend observes a new one (`DelegatedManager.saveResumeID`), so
  "which CC session was live at T" survives resets and respawns.

```go
idx.ArchiveFileAt("clutch/c123", t)  // earliest archive rotated at/after t; miss = live file
idx.CCResumeAt("clutch/c123", t)     // newest resume-ID observation at/before t
store.ArchiveFileAt("clutch/c123", t) // same answer from filename stamps alone
```

From the CLI: `foci debug at clutch/c123 2026-07-01T12:00:00Z` (or a
duration ago, e.g. `foci debug at clutch 3h`) prints the covering JSONL path
and the CC resume ID live at that moment.

### Migrating pre-stable-identity installs

Both migrations run automatically at startup and are idempotent:

- `Store.MigrateLegacyLayout` flattens version directories: the newest
  version's `root.jsonl` becomes the live file; older versions become
  in-place archives stamped with the *superseding* version's timestamp (the
  moment they were rotated away); children move up beside them with their
  `branch_meta` parent keys rewritten.
- `migrateLegacyStateDB` (inside `NewSessionIndex`) re-keys
  `session_metadata` (newest version wins — this carries `cc_resume_id`
  forward), converts `chat_metadata` `session_key` rows to `registered`
  ownership rows (preserving app named-session adoptions), re-keys
  facet/tmux `agent_metadata` values, and clears legacy `session_index` rows
  plus the clean-shutdown marker so the index rebuilds from the migrated
  files.

### Index resolution

The session index stores structured columns (`agent_id`, `chat_id`, `is_root`)
derived from the key at insert, so resolution is SQL over columns, never string
pattern-matching:

```go
idx.DefaultSessionKeyForAgent("main") // default chat, else most-recent active root
idx.ResolveLooseKey("main")           // bare agent name → default session key
idx.PlatformForChat("main", 123)      // which platform owns this chat
idx.SessionExists(key)
```
