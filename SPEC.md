# Clod — Specification

A minimal, maintainable agent platform in Go. One binary, no framework, no bloat.

## Philosophy

- **Simple over powerful.** If a feature needs complex config, rethink the feature.
- **Explicit over clever.** No plugin architectures, no hook systems, no middleware chains.
- **Own every line.** No 5.4GB node_modules. Dependencies are standard library + a few well-chosen packages.
- **Cache-aware from day zero.** Anthropic prompt caching drives architectural decisions.

## Architecture

### Session Keys

Format: `agent:AGENTID:TYPE[:BRANCHID]`

- `agent:main:main` — primary DM session
- `agent:main:cron:morning-routine` — cron-triggered branch
- `agent:main:subagent:research-task` — sub-agent branch

The 4th segment (branch ID) is optional. Branch sessions inherit the parent's message prefix for cache sharing.

### Session Branching (Cache Sharing)

A branch session copies the parent's system prompt + message history at a point in time. When sent to Anthropic, the shared prefix hits the cache (read pricing: $0.30/MTok on Haiku, $0.50/MTok on Opus) instead of being rewritten ($1.25/MTok Haiku, $6.25/MTok Opus).

**Rules:**
- Parent session: append-only, owns canonical history
- Branch session: snapshot of parent messages at branch point + own appended messages
- Branch never writes back to parent history
- Branch result delivered as a message to the parent session or via Telegram
- System prompt MUST be byte-identical between parent and branch for cache hit

**Storage:** A branch record holds:
```
parent_key: "agent:main:main"
branch_point: <message index>
messages: [only messages after branch point]
```

API payload assembly: system prompt + parent.messages[:branch_point] + branch.messages

### Anthropic API

- **Auth:** Subscription token (OAuth-style `sk-ant-oat01-...`)
- **Model:** Haiku (`claude-haiku-4-5`) for clod itself; configurable per agent
- **Prompt caching:** `cache_control` with `{"type": "ephemeral"}` on system prompt blocks
- **Streaming:** Server-sent events for responses
- **Key constraint:** System prompt must be byte-identical across turns for cache reuse

### Telegram Bot

- Long-polling (not webhooks) for simplicity
- Receive: text messages, file attachments (beta)
- Send: text messages, markdown formatting, file attachments (beta)
- Route incoming messages to the correct agent session
- DM only for alpha; group chat support in beta

### Tool System

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery.

**Alpha tools:**
- `exec` — run shell commands (with timeout, background support)
- `read` — read file contents
- `write` — create/overwrite files
- `edit` — find-and-replace in files
- `web_fetch` — HTTP GET, extract readable content
- `web_search` — Brave Search API
- `memory_search` — grep-based search over memory files

**Each tool is a function with signature:**
```go
type Tool struct {
    Name        string
    Description string
    Parameters  json.RawMessage  // JSON Schema
    Execute     func(ctx context.Context, params json.RawMessage) (string, error)
}
```

### Workspace Bootstrap

On session start, read markdown files from the workspace directory and inject them as system prompt blocks. Files are read in a fixed order (configurable in TOML):

```
IDENTITY.md, SOUL.md, COHERENCE.md, AGENTS.md, TOOLS.md, USER.md, MEMORY.md, HEARTBEAT.md
```

Order matters: most-stable files first maximises cache prefix length.

### Memory System

**Alpha:** File-based with grep search.
- Memory files in `workspace/memory/YYYY-MM-DD.md`
- Curated long-term memory in `workspace/MEMORY.md`
- Search: grep across all `.md` files in memory directory
- Injected into system prompt on each turn

**Beta:** Add vector embeddings (OpenAI via OpenRouter), hybrid search, temporal decay.

### Compaction

**Alpha:** Simple threshold-based.
- When context exceeds N% of model's context window, trigger compaction
- Pre-compaction: inject system message "save important context to memory now", let agent write to memory files
- Compaction: call model with "summarise this conversation" prompt, replace history with summary
- Post-compaction: inject handoff note so agent knows compaction occurred

**Beta:** Custom identity-aware prompts, configurable thresholds, retry logic.

### Heartbeat

A timer that fires when the session has been idle for a configurable duration.

- Injects a heartbeat message into the session
- Agent processes it like any other turn (reads HEARTBEAT.md, decides what to do)
- If agent responds with `HEARTBEAT_OK`, no action taken
- Configurable interval (default: 45 minutes)

### Cron

**Alpha:** Use system crontab. A tiny HTTP endpoint accepts wake messages:
```
POST /wake
{"agent": "main", "text": "morning routine"}
```

This injects the text as a user message into a branch session of the specified agent.

**Beta:** Built-in cron scheduler if system crontab proves limiting.

### Secrets

Secrets never pass through agent context. The agent cannot read, echo, or exfiltrate credentials.

### Principle
Credentials are loaded once at startup into process memory. Built-in integrations (Anthropic, Telegram, Brave Search) use them directly from Go structs. The agent interacts with tools, tools use credentials internally — the agent never constructs auth headers or sees token values.

### Architecture

**`secrets.toml`** — separate from main config, `0600` permissions, read once at startup.

```toml
# secrets.toml — loaded at startup, never accessible to agent

[anthropic]
token = "sk-ant-oat01-..."

[telegram]
bot_token = "8351531463:AAH..."

[brave]
api_key = "BSA..."

# Ad-hoc secrets for exec template references
[custom]
github_token = "ghp_..."
openrouter_key = "sk-or-v1-..."
```

**Three layers:**

1. **Built-in integrations** — Anthropic client, Telegram bot, etc. receive credentials via Go structs at init. Agent calls tools; tools use credentials internally. Zero exposure.

2. **Exec template references** — For ad-hoc commands the agent can reference secrets by name:
   ```
   curl -H "Authorization: Bearer {{secret:custom.github_token}}" https://api.github.com/...
   ```
   Clod resolves `{{secret:NAME}}` before spawning the subprocess. The agent sees the template, never the value. Unresolved references are an error (not silently passed through).

3. **Output redaction** — Exec tool output is scanned for known secret patterns and redacted before returning to the agent. Defence in depth — catches accidental leaks from `env`, error messages, config dumps, etc.

### Blocked paths
The exec tool refuses to read `secrets.toml`, `/proc/self/environ`, and any path matching a configurable blocklist. Not adversarial defence — the agent isn't hostile, just careless.

### What the agent knows
- That secrets exist (by name): "anthropic", "telegram", "brave", "custom.github_token"
- How to reference them: `{{secret:NAME}}`
- Nothing about their values

## Config

Single TOML file. Flat, commented, no deep nesting.

```toml
# clod.toml

[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/home/rich/git/openclaw/workspace"
heartbeat_interval = "45m"

[anthropic]
token = "sk-ant-oat01-..."

[telegram]
bot_token = "8351531463:AAH..."
allowed_users = ["5970082313"]

[sessions]
dir = "/home/rich/git/clod/sessions"
compaction_threshold = 0.8  # compact at 80% context usage

[memory]
dir = "/home/rich/git/openclaw/workspace/memory"

[http]
port = 18790
bind = "127.0.0.1"
```

## Alpha Scope

**In:**
- Anthropic API client with prompt caching
- Session store (JSONL-backed)
- Session branching with cache sharing
- Telegram bot (text messages, DM only)
- Core tools: exec, read, write, edit, web_fetch, web_search, memory_search
- Workspace bootstrap (markdown files → system prompt)
- Heartbeat
- Simple compaction
- Basic memory (grep-based)
- TOML config
- Wake endpoint for cron

**Out (beta):**
- Vector memory search
- Multi-agent support (design for it, don't build it)
- Sub-agents
- File attachments
- Skill framework
- Signal/Discord/other channels

**Out (maybe never):**
- Built-in cron scheduler
- Plugin/hook architecture
- Reactions
- Config schema validation beyond basic TOML parsing

## Testing Priority

**Critical first test:** Session branching cache sharing.
1. Create a session with a system prompt + several messages
2. Send a request → observe cache write
3. Send another request on same session → observe cache read
4. Create a branch from this session
5. Send a request on the branch → observe cache READ (not write) for shared prefix
6. Send another request on parent → observe parent cache still works (branch didn't bust it)

If step 5 shows a full cache write instead of a read, the branching architecture doesn't work and we need to rethink.

## Dependencies (Go)

Minimal:
- `github.com/BurntSushi/toml` — config parsing
- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram (or hand-roll, it's just HTTP)
- Standard library for everything else (net/http, encoding/json, os/exec, etc.)

## Directory Structure

```
clod/
├── SPEC.md
├── clod.toml
├── go.mod
├── go.sum
├── main.go              # entry point, wire everything together
├── anthropic/           # API client, streaming, caching
│   ├── client.go
│   ├── types.go
│   └── cache_test.go   # THE critical test
├── session/             # session store, branching
│   ├── store.go
│   ├── branch.go
│   └── store_test.go
├── telegram/            # bot, message routing
│   └── bot.go
├── tools/               # tool implementations
│   ├── registry.go
│   ├── exec.go
│   ├── files.go
│   ├── web.go
│   └── memory.go
├── workspace/           # bootstrap file loading
│   └── bootstrap.go
├── compaction/          # simple compaction
│   └── compact.go
└── config/              # TOML config loading
    └── config.go
```

---

_This spec describes what we're building, not how to build it. Implementation decisions belong in the code._
