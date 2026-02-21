# Clod — Wiring Diagram

How the pieces connect. Read this before touching the code.

## Startup Flow (`main.go`)

```
config.Load(path)
  → log.Init(cfg.Logging)
  → anthropic.NewClient(token)
  → session.NewStore(dir)
  → tools.NewRegistry() + register all tools
  → workspace.NewBootstrap(dir, fileOrder)
  → agent.Agent{Client, Sessions, Tools, Bootstrap, Model}
  → telegram.NewBot(token, allowedUsers, agent, sessionKey)  → goroutine
  → agent.NewHeartbeat(agent, sessionKey, interval)           → goroutine
  → http.Server{"/wake" handler}                              → goroutine
  → signal.Notify(SIGINT, SIGTERM) → shutdown
```

## Package Dependency Graph

```
main
 ├── config        (no deps)
 ├── log           (no deps)
 ├── anthropic     (no deps)
 ├── session       → anthropic
 ├── tools         → anthropic, log
 ├── workspace     → anthropic
 ├── compaction    → anthropic, session, log
 ├── agent         → anthropic, session, tools, workspace, log
 └── telegram      → agent, log
```

No circular dependencies. `log` and `config` are leaf packages.

## The Agent Loop (`agent/agent.go`)

The core of the system. `HandleMessage(ctx, sessionKey, userMessage)`:

```
1. sessions.LoadFull(sessionKey)          ← parent[:branchPoint] + own msgs
2. append user message
3. bootstrap.SystemBlocks()               ← workspace/*.md → []SystemBlock
4. tools.ToolDefs()                       ← registry → []ToolDef
5. LOOP (max 25 iterations):
   a. client.SendMessage(system, messages, tools)
   b. log event + log API entry
   c. if stop_reason == "end_turn" → save & return text
   d. if stop_reason == "tool_use":
      - execute each tool via registry
      - append assistant msg + tool_result msg
      - goto 5a
6. sessions.AppendAll(sessionKey, newMessages)
```

Messages are only saved to disk after the full turn completes (all tool loops resolved).

## Session Storage

**Format:** JSONL files, one JSON-encoded `anthropic.Message` per line.

**Key → Path mapping:**
```
agent:main:main           → {dir}/agent/main/main.jsonl
agent:main:cron:morning   → {dir}/agent/main/cron/morning.jsonl
```

**Branching:** Branch files start with a `{"type":"branch_meta",...}` line containing `parent_key` and `branch_point`. `LoadFull()` reads parent[:branch_point] + branch's own messages. This is what makes cache sharing work — the API sees the same prefix bytes.

## System Prompt Assembly (`workspace/bootstrap.go`)

Reads markdown files from workspace dir in order:
```
IDENTITY.md → SOUL.md → COHERENCE.md → AGENTS.md → TOOLS.md → USER.md → MEMORY.md → HEARTBEAT.md
```

Each becomes a `SystemBlock{type:"text", text:content}`. The **last** block gets `cache_control: {type: "ephemeral"}`. Order matters: most-stable files first maximizes cache prefix reuse.

Missing/empty files are silently skipped.

## Prompt Caching

Two cache breakpoints per API request:

1. **System prompt** — `cache_control: ephemeral` on the last `SystemBlock` (set by `bootstrap.SystemBlocks()`). Caches the entire system prompt so it's not re-tokenized each turn.

2. **Conversation history** — `cache_control: ephemeral` on the last content block of the second-to-last message (set by `withCacheBreakpoint()` in `agent.go`). Caches system prompt + conversation history up to the previous turn.

Cache breakpoints are added **only to the API request payload**, never persisted to session storage. The `withCacheBreakpoint()` function returns a shallow copy of the messages slice.

**Branch cache sharing:** When a branch session's `LoadFull()` builds a message list starting with the parent's prefix, the cache breakpoint lands on the same byte-identical prefix. The API hits cache (read pricing) instead of re-tokenizing (write pricing).

**Requirements for cache hits:**
- System prompt must be byte-identical across turns (workspace files don't change mid-conversation)
- `anthropic-beta: prompt-caching-2024-07-31` header (set in `client.go`)
- OAuth tokens also need `oauth-2025-04-20` in the beta header

**Verify in `api.jsonl`:** `cache_read > 0` on the second message in a session means caching is working.

## Secrets (`secrets/`)

Loaded from `secrets.toml` (same directory as `clod.toml`). Format:

```toml
[anthropic]
token = "sk-ant-oat01-..."

[telegram]
bot_token = "123:ABC"

[custom]
github_token = "ghp_..."
```

Stored as flat keys: `anthropic.token`, `custom.github_token`, etc. Overrides `clod.toml` credentials at startup.

Features:
- **Template resolution:** `{{secret:custom.github_token}}` in exec commands → replaced with actual value before execution
- **Output redaction:** Secret values in command output → `[REDACTED]` (skips values < 4 chars)
- **Path blocking:** Commands referencing `secrets.toml` or `/proc/self/environ` are refused

## Logging (`log/`)

Two outputs:

1. **Event log** (`clod.log` + stderr): `2026-02-21T03:52:39Z INFO  [telegram] message from rich: hello`
   - Use: `log.Infof("component", "format", args...)`
   - Levels: DEBUG < INFO < WARN < ERROR

2. **API log** (`api.jsonl`): One JSON object per Anthropic API call with ts, session, model, token counts, cost_usd, duration_ms.
   - Use: `log.API(log.APIEntry{...})`
   - Queryable with `jq`

## Tool System (`tools/`)

Each tool is a `Tool` struct with `Execute func(ctx, params) (string, error)`. Registry maps name → tool. Tools available:

| Tool | File | What it does |
|------|------|-------------|
| `exec` | exec.go | Shell commands via `sh -c`, process group kill on timeout |
| `read` | files.go | File contents with line numbers, truncates at 2000 lines |
| `write` | files.go | Create/overwrite files |
| `edit` | files.go | Find-and-replace (old_string must be unique) |
| `web_fetch` | web.go | HTTP GET, strip HTML tags |
| `web_search` | web.go | Brave Search API |
| `memory_search` | memory.go | Grep across .md files in memory dir |

## Config (`config/config.go`)

Single `clod.toml` parsed with BurntSushi/toml. Sections: `[agent]`, `[anthropic]`, `[telegram]`, `[sessions]`, `[memory]`, `[http]`, `[logging]`. Defaults applied for missing fields.

## Telegram Bot (`telegram/bot.go`)

Long-polling loop. Filters by `allowed_users` (string user IDs). Sends typing indicator while agent processes. Splits responses at 4096 chars. Falls back to plain text if markdown parsing fails.

## Heartbeat & Wake

- **Heartbeat** (`agent/heartbeat.go`): Timer goroutine, fires after idle duration, injects `[HEARTBEAT]` message into main session. Resets on any activity.
- **Wake** (in `main.go`): `POST /wake` creates a branch session from the agent's main session, injects the text, runs the agent on the branch.

## Compaction (`compaction/compact.go`)

Checks token usage against threshold (default 80% of 200k). When triggered: asks model to summarize history, replaces session with 3-message compacted version (context note + summary + continuation note).

## Testing

```
go test ./...           # all tests (~66, runs in ~1s)
go test ./... -v        # verbose
go test ./session/...   # single package
```

The cache_test.go in `anthropic/` requires `ANTHROPIC_API_KEY` env var and hits the real API. All other tests are self-contained.
