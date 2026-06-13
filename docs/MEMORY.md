# Memory System

Foci's memory system provides full-text search over markdown files, automatic memory formation, and periodic consolidation. It supports multiple source directories with weighted relevance, two search backends, and per-agent isolation.

---

## Overview

Memory has four layers:

1. **Sources** — directories of `.md` files indexed for search
2. **Search backends** — FTS5 (SQLite) and Bleve (pure-Go full-text search)
3. **Memory formation** — automatic capture of session learnings to daily files
4. **Memory consolidation** — periodic curation of `MEMORY.md` from daily files

Files are indexed at startup, watched for changes via fsnotify, and periodically swept to catch files added by external tools (git, rsync). Conversation history is also indexed for search.

---

## Memory Sources

Sources are directories of markdown files that get indexed. Each source has a name, directory path, and weight that influences search ranking.

### Configuration

Global sources in `foci.toml`:

```toml
[[memory.sources]]
name = "canonical"
dir = "/home/foci/character/memory"
weight = 1.0

[[memory.sources]]
name = "docs"
dir = "/home/foci/project/docs"
weight = 0.5
```

Per-agent sources in `[[agents]]`:

```toml
[[agents.memory.sources]]
name = "workspace"
dir = "/home/foci/myagent/memory"
weight = 1.0
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | required | Unique source identifier (e.g., `"canonical"`, `"docs"`). |
| `dir` | string | required | Directory path to index. All `.md` files are indexed recursively. |
| `weight` | float | `1.0` | Relevance weight (0.0–1.0). Higher values rank results from this source higher. |

### Weight Formula

Search results are ranked using: `rank * (1.0 + weight)`

- A source with `weight = 1.0` gets a 2.0x multiplier
- A source with `weight = 0.5` gets a 1.5x multiplier
- Conversation results use a separate `conversation_weight` (default `0.1`), so memory files rank well above conversation matches

### Per-Agent Sources

When any agent has per-agent memory sources, each agent gets its own search index combining global + agent-specific sources. Agent-specific sources automatically receive a **+1.0 weight boost**, so they rank higher than global sources with the same base weight. Source names are prefixed with `agent/` in search results.

Example: an agent source with `weight = 0.5` gets an effective multiplier of `1.0 + (0.5 + 1.0) = 2.5`.

If no agent defines per-agent sources, all agents share a single index.

Default per-agent source (when none configured): `{name: $id, dir: $workspace/memory, weight: 1.0}`.

---

## Search Backends

Two backends are available. They can run independently or simultaneously.

### FTS5 (SQLite)

The default backend. Uses SQLite's FTS5 extension with Porter stemming and unicode61 tokenization.

- Zero external dependencies (built into SQLite)
- Indexes both memory files and conversation history in real-time
- Deterministic ranking via SQL
- Snippet generation: 40-character context with `>...<` markers
- Database: `memory.db` (shared mode) or `memory-{agentID}.db` (per-agent mode)

### Bleve

Pure-Go full-text search with English analyzer and Porter stemming.

- Richer analysis pipeline with term vectors and highlighting
- Indexes memory files, conversation history, and todo items
- Backfills historical conversation on startup from SQLite conversation database
- HTML highlighting converted to `>...<` markers for consistency with FTS5
- Index directory: `search.bleve/` (shared or per-agent)

### Configuration

```toml
[memory]
search_backends = ["fts5"]           # or ["bleve"], or ["fts5", "bleve"]
conversation_weight = 0.1            # weight for conversation results (0.0–1.0)
search_limit = 20                    # max results returned
reindex_debounce = "0s"              # delay before reindex on file change
sweep_interval = "1h"                # periodic full reindex interval ("0" disables)
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `search_backends` | string[] | `["fts5"]` | Active backends: `"fts5"`, `"bleve"`, or both. |
| `conversation_weight` | float | `0.1` | Weight multiplier for conversation results. Lower values push conversation below memory files. |
| `search_limit` | int | `20` | Maximum search results returned per query. |
| `reindex_debounce` | string | `"0s"` | Delay before reindex after file changes. Go duration format. |
| `sweep_interval` | string | `"1h"` | Periodic full reindex interval. Catches files added by git, rsync, or other external tools. First sweep runs 30s after startup. `"0"` disables. |

When multiple backends are active, the `memory_search` tool exposes a `backend` parameter so the agent can choose which to query. When only one is active, the parameter is hidden.

---

## The `memory_search` Tool

Full-text search with natural language queries and stemming support. Memory files are ranked higher than conversation history.

### Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `query` | string | yes | Search query. Supports natural language with Porter stemming (e.g., "running" matches "run"). |
| `sort` | string | no | `"relevance"` (default), `"newest"`, or `"oldest"`. Relevance uses weighted ranking; newest/oldest sort by file modification time. |
| `date_from` | string | no | Filter results on or after this date (YYYY-MM-DD, inclusive). |
| `date_to` | string | no | Filter results on or before this date (YYYY-MM-DD, inclusive — internally converted to start of next day as exclusive bound). |
| `backend` | string | no | Which backend to query. Only shown when multiple backends are active. |

### Result Format

```
[source] path: snippet
```

Examples:
```
[canonical] 2024-01-15.md: >Session planning: created agents from IDENTITY templates.<
[docs] architecture.md: >Memory system uses FTS5 or bleve for full-text search<
[conversation] main/c123/ts456: >We discussed memory implementation yesterday<
[agent/workspace] notes.md: >Deploy script updated for new staging environment<
```

---

## File Indexing

### Supported Format

Only `.md` (markdown) files are indexed. Files are scanned recursively within each source directory.

### Indexing Triggers

1. **Startup** — full scan of all source directories
2. **File changes** — fsnotify watcher triggers reindex on `.md` file create/write/remove, debounced by `reindex_debounce`
3. **Periodic sweep** — full reindex at `sweep_interval` (default hourly, first at 30s after startup)

The sweep exists to catch files added by external mechanisms that bypass fsnotify (git pull, rsync, etc.).

### Conversation Indexing

Conversation messages are indexed as they arrive:
- **FTS5**: real-time indexing only (no historical backfill)
- **Bleve**: real-time indexing plus startup backfill from the SQLite conversation database

---

## Memory Formation

Automatic capture of session learnings into daily memory files (`memory/YYYY-MM-DD.md`). Runs inside the wider **reflection pass** at `shared/prompts/reflection.md`, which also nudges the agent to turn replayable workflows into autogenerated skills (see [Skill Formation](#skill-formation) below).

### What Gets Captured

- Decisions made and their reasoning
- Lessons learned (especially mistakes)
- Project milestones
- Things future sessions need to know

### What Doesn't Get Captured

- Play-by-play transcripts
- Individual commits, tool calls, or command outputs
- Intermediate states
- Duplicates of existing entries

### Triggers

Formation runs in three modes, each independently configurable:

**Interval** — periodic capture on a timer. Fires when:
1. `interval` has elapsed since the last formation
2. There's been user activity since the last formation
3. The user has been active within the interval window

**Session-end** — runs asynchronously on `/reset` and facet reclaim. Creates a branch from the expiring session (preserving conversation history) so the caller doesn't block.

**Consolidation** — curates `MEMORY.md` from recent daily files (covered in next section).

### Skill Formation (Same Pass)

The reflection prompt also instructs the agent to capture *procedural* knowledge as autogenerated skills when it notices a workflow worth replaying (5+ tool calls, error recovery, user correction, non-obvious sequence). New skills are written to `workspace/skills/{slug}/SKILL.md` with `autogenerated: true` in frontmatter — a human removes that flag after review. See the Skills section in [SPEC.md](SPEC.md#skills) for the full lifecycle.

### Configuration

Set globally in `[reflection]` or per-agent in `[[agents.reflection]]`.

```toml
[reflection]
interval_enabled = true
interval = "1h"
session_end_enabled = true

[maintenance]
consolidation_enabled = true
consolidation_time = "20h"   # or "04:00" for daily at 4am
reset_time = ""              # e.g. "04:20" for a daily soft /reset
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `interval_enabled` | bool | `true` | Enable periodic memory capture on timer. |
| `interval` | string | `"1h"` | Time between interval captures. |
| `interval_prompt` | string | `""` | Prompt override. `""` = embedded default, `"none"` = disabled, file path = custom prompt. |
| `session_end_enabled` | bool | `true` | Run memory formation on `/reset` and facet reclaim. |
| `session_end_prompt` | string | `""` | Prompt override (same 3-state resolution). |

Consolidation and the daily reset live in `[maintenance]` (or per-agent `[[agents.maintenance]]`):

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `consolidation_enabled` | bool | `true` | Enable periodic MEMORY.md curation. |
| `consolidation_time` | string | `"20h"` | `"HH:MM"` daily or a duration like `"20h"`. Persisted across restarts. |
| `consolidation_prompt` | string | `""` | Prompt override (same 3-state resolution). |
| `reset_time` | string | `""` | Daily soft `/reset`: `"HH:MM"`, a duration, or `""` to disable. |
| `reset_idle_guard` | string | `"55m"` | Skip the scheduled reset if the user was active within this window. |

### Prompt Customization

Each formation trigger has its own prompt field (`interval_prompt`, `session_end_prompt`, `consolidation_prompt`). All use the same 3-state resolution:

| Value | Behavior |
|-------|----------|
| `""` or `"default"` | Use the embedded prompt from the `shared/prompts/` directory. |
| `"none"` | Disable this trigger entirely. |
| `/path/to/file.md` | Use the custom file as the prompt. Falls back to the embedded prompt on read error. |

Source: `internal/periodic/keepalive.go` (`prompts.ResolvePrompt`).

---

## Memory Consolidation

Periodic curation of `MEMORY.md` from recent daily memory files. Uses the prompt at `shared/prompts/memory-consolidation.md`.

### What Goes Into MEMORY.md

- Long-lived facts (preferences, conventions, system setup)
- Lessons learned that will apply again
- Ongoing projects and commitments
- Important relationships or context

### What Stays in Daily Files

- What was worked on this week
- Completed one-off tasks
- Technical details of specific fixes
- Session-specific context

### Size Management

`MEMORY.md` has a hard limit of **20,000 characters**. When it exceeds 15,000 characters, the consolidation process prunes completed projects and stale context by moving them to dated memory files before adding new content.

### When It Runs

Consolidation fires when:
1. `consolidation_time` is due — either the daily `"HH:MM"` clock time has arrived, or the configured duration has elapsed since the last run
2. The user has been active within the last hour

The last-run timestamp is persisted in state, so it survives restarts. Configured under `[maintenance]` (was `[reflection].consolidation_interval` before the rename).

---

## Database Locations

### Shared Mode (no per-agent sources)

```
$data_dir/
  memory.db           ← FTS5 index
  search.bleve/       ← Bleve index
```

### Per-Agent Mode (any agent has per-agent sources)

```
$workspace/.data/
  memory.db           ← FTS5 index (agent-specific)
  search.bleve/       ← Bleve index (agent-specific)
  scratchpad.db       ← scratchpad store
  todo.db             ← todo store
  reminders.db        ← reminder store
  tasklist.db         ← task list store
```

---

## Related Stores

These stores live alongside memory but serve distinct purposes:

| Store | Tool | Description |
|-------|------|-------------|
| **Scratchpad** | `scratchpad` | Key-value working notes that survive compaction. Per-agent. |
| **Todo** | `todo` | Task list with priority, status tracking, and full-text search. Per-agent with sequential IDs. When Bleve is enabled, todo search uses Bleve instead of the embedded FTS5 index (see `TodoStore.SetSearchIndex()`). |
| **Reminders** | `remind` | Deferred thoughts surfaced when due. Two modes: **passive** (default) — injected as context when due, dismissed in batch; **wake** — actively wakes the session at the scheduled time, dismissed individually by ID (see `ReminderStore.AddWake()`). |
| **Task List** | n/a | Collaborative task tracking across sessions. |

See [TOOLS.md](TOOLS.md) for tool interface details.
