<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Databases & persistent state

Foci keeps state in SQLite, split into two scopes. As an agent you rarely query these directly — your tools (`foci_todo`, `foci_remind`, `foci_memory_search`) are the front door — but knowing the layout helps when reasoning about what persists.

## Two scopes

- **Global** (`~/data/` by default, configurable via `data_dir`): shared across all agents.
- **Per-agent** (`<workspace>/.data/`): one set per agent.

## Global (`~/data/`)

| Database | Table(s) | Contents |
|----------|----------|----------|
| `api.db` | `api_calls` | Every model API call: timestamp, cost (list-price equivalent), cache read/write tokens. The basis for "where did the mana go". |
| `state.db` | `session_index`, `agent_metadata`, `chat_metadata`, `session_metadata`, `system_state` | Unified runtime state: session lifecycle, per-agent/chat/session metadata. |
| `sessions/` | (JSONL files, not a DB) | Per-session conversation history: `~/data/sessions/<AGENT_ID>/c<CHAT_ID>/<VERSION_TS>/root.jsonl`. |

## Per-agent (`<workspace>/.data/`)

| Database | Table(s) | Contents |
|----------|----------|----------|
| `conversation.db` | `messages` | Telegram/Discord send/receive log (NOT the session history — that's the JSONL store). |
| `todo.db` | `todos` | Todo items (`foci_todo`). |
| `tasklist.db` | `tasks` | Task-list items (the Task* tools). |
| `scratchpad.db` | `scratchpad` | Working notes. |
| `reminders.db` | `reminders` | Scheduled reminders (`foci_remind`). |
| `tool_details.db` | `tool_call_details` | Tool-call display data for inline buttons. |
| `memory.db` | `memory_fts`, `memory_meta` | FTS5 full-text index over your memory sources (powers `foci_memory_search`). |
| `search.bleve/` | — | Bleve search index (alternative search backend). |

## Memory: files are the source of truth

The databases above are indexes and logs; your **durable memory is files**:

- `character/MEMORY.md` — curated long-term memory, part of the system prompt.
- `memory/YYYY-MM-DD.md` — daily logs; raw session notes. Indexed into `memory.db` for `foci_memory_search`.
- Memory **sources** (which dirs are indexed, and their search weights) are set per-agent in config (`memory.sources`); a reflection pass writes to the daily file, and consolidation curates `MEMORY.md` from the dailies.

For deeper debugging — schemas, cost analysis, session tracing — see the `foci-debugging` skill.