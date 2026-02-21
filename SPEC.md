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
- Receive: text messages, voice notes, file attachments (beta)
- Send: text messages, markdown formatting, voice notes, file attachments (beta)
- Route incoming messages to the correct agent session
- DM only for alpha; group chat support in beta

### Multi-Bot Sessions (/multiball)

An agent can have multiple Telegram bots assigned — one primary, the rest secondary. Secondary bots are idle until needed.

```toml
[telegram]
bot_token = "primary-bot-token"
allowed_users = ["5970082313"]
secondary_bots = ["secondary-bot-token-1", "secondary-bot-token-2"]
```

**`/multiball` (alias `/mb`):**
1. Fork the current session (cache-sharing branch)
2. Attach the fork to the least-recently-used secondary bot
3. That bot sends the user a Telegram message: "🎱 Forked from main. What do you need?" (plain Telegram message, not an agent turn — no tokens spent)
4. All subsequent messages to that bot route to the forked session

The user now has two (or more) parallel conversations with the same agent, each in its own Telegram chat, sharing the cached prefix. When the fork is done, `/done` in that chat detaches the bot and returns it to the pool.

**Why:** Sometimes you want to ask a side question without derailing the main conversation. Or run parallel investigations. Each gets its own chat window — no interleaving, no confusion.

### Voice (Telegram Voice Notes)

**Inbound:** Receive Telegram voice notes → transcribe via Whisper API (OpenAI-compatible, via OpenRouter or local) → inject transcript as the user message with a `[voice]` tag. The agent sees text, doesn't need to handle audio.

**Outbound:** Agent can send voice replies via a `tts` tool. Text → TTS engine (Edge TTS or similar, free) → send as Telegram voice note. Good for when the human is mobile/driving.

**Voice mode:** A session-level flag toggled by the user ("voice mode on"/"voice mode off"). When active:
- All agent replies are sent as voice notes via TTS
- The flag is included in message metadata: `voice=on`
- The agent sees this and adjusts style — shorter, conversational, no markdown, no code blocks

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m voice=on model=claude-haiku-4-5 prev_cost=$0.043
```

Voice mode is session state, not config — it resets on session reset.

### Message Metadata

Each user message injected into the conversation carries metadata the agent can see. This is NOT in the system prompt (that would bust cache) — it's prepended to the user message content.

```
[meta] time=2026-02-21T05:30:00Z gap=3h12m model=claude-haiku-4-5 prev_cost=$0.043 prev_tokens=in:2400/out:312/cR:18000/cW:200
```

Fields:
- `time` — current UTC timestamp
- `gap` — time since the previous message in this session (human-readable: "3h12m", "2d4h", "38s")
- `model` — current model name (so the agent knows its own capabilities)
- `prev_cost` — total cost of the previous agent turn (API call that generated the last response)
- `prev_tokens` — token breakdown of the previous turn (input/output/cache_read/cache_write)

**Why metadata on messages, not system prompt:** The system prompt must be byte-identical across turns for cache reuse. Dynamic values like timestamps go on messages instead — they're past the cache breakpoint.

**Why previous turn's cost, not current:** The current turn's cost isn't known until after the API responds. So each message carries the cost of the turn that came before it. The agent always knows what its last response cost.

### Deferred Replies

The agent can acknowledge a message and deliver a full response later. For complex questions requiring research or long tool chains:

1. Agent sends an immediate short reply ("Looking into this, give me a minute")
2. Agent continues working (tool calls, research, etc.)
3. Agent sends the full response when ready

Implementation: The agent turn can produce multiple Telegram messages. The first is sent immediately. Subsequent messages are sent as the agent completes tool calls. This is just streaming tool results to Telegram rather than batching everything into one final response.

### Scratchpad

Working notes that survive compaction but aren't permanent memory. For when the agent is mid-investigation and building up context that would be catastrophic to lose but isn't worth saving to memory files.

Tools:
- `scratchpad_write(text)` — append to scratchpad
- `scratchpad_read()` — return current scratchpad contents
- `scratchpad_clear()` — empty the scratchpad

Stored in SQLite. On compaction, scratchpad contents are injected back into the post-compaction context as a system message. The agent is responsible for clearing it when done — it's working state, not knowledge.

### Model Escalation

No dedicated mechanism. Use `request_model` — which is just a synchronous subagent call with a custom prompt, no tools, and no cache writing:

```
request_model("opus", "Evaluate whether this cache-sharing architecture has a subtle correctness bug: [full context here]")
```

The agent constructs a self-contained prompt with all necessary context. Opus (or whatever model) gets a one-shot cold call, responds, and the result comes back as a tool result. The session stays on its original model with its warm cache intact.

This is syntactic sugar over the subagent system — same dispatch path, just synchronous and tool-less.

### Subagent Prompt Weight

Subagents (including `request_model`) accept a prompt weight that controls how much system context they inherit:

- **`full`** — character files (identity, soul, agents, tools, memory) + parent's payload. Default for background tasks that need to "be you."
- **`light`** — minimal system prompt + parent's payload. Default for `request_model`. You're asking a question, not spawning a personality.
- **`none`** — parent's payload only. Zero overhead. Parent is 100% responsible for context.

This keeps model escalation cheap — an Opus consultation with `light` or `none` prompt is just the question + context the agent packs, not 15k tokens of character files.

### Thought Queue

The agent can defer thoughts for later via a `memory_remind` tool:

```
memory_remind("Look into whether FTS5 supports phrase boosting", "next_heartbeat")
memory_remind("Ask Dick about the Greece decision", "tomorrow")
```

Reminders surface as injected context at the specified time (next heartbeat, next session, specific date). Stored in SQLite alongside conversation log. Lightweight — not a full task system, just "future me should think about this."

### Tool System

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery.

**Alpha tools:**
- `exec` — run shell commands (with timeout, background support)
- `read` — read file contents
- `write` — create/overwrite files
- `edit` — find-and-replace in files
- `web_fetch` — HTTP GET, extract readable content
- `web_search` — Brave Search API
- `memory_search` — FTS5 search over memory files + conversation history
- `memory_remind` — defer a thought for later (next heartbeat, tomorrow, specific date)
- `request_model` — escalate to a heavier model for the next turn

### Tool Result Guard

When a tool returns a result exceeding a configurable character threshold (default: 10,000 chars), clod does NOT inject the full result into session history. Instead:

1. Write the full result to a temp file in the agent's home dir: `/home/clod/tmp/tool-result-{tool}-{timestamp}.txt`
2. Return a truncated result to the agent with instructions:
   ```
   [Result too large: 47,231 chars. Full output saved to /home/clod/tmp/tool-result-exec-1708512345.txt]
   Use `read` tool to inspect specific sections. First 500 chars:
   ...
   ```

This prevents large tool results (e.g. `exec cat bigfile.txt`) from permanently bloating session history until compaction. The agent can still access the full result via `read` — it just doesn't sit in context forever.

```toml
[tools]
result_guard_chars = 10000   # max chars before writing to file
```

Reference implementation: OpenClaw's `session-tool-result-guard-wrapper.ts` (`~/git/openclaw_code/src/agents/session-tool-result-guard-wrapper.ts`)

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

### Skills

Skills extend the agent without code changes. A skill is a directory containing a `SKILL.md` file with YAML frontmatter and markdown instructions:

```
/home/clod/skills/
├── reheat/
│   ├── SKILL.md
│   └── reheat.sh
└── research/
    └── SKILL.md
```

**SKILL.md format:**
```yaml
---
name: reheat
description: Clear API cooldowns
command: /reheat
script: reheat.sh
---

Instructions the agent follows when this skill is activated.
The agent reads this file with the `read` tool.
```

**Frontmatter fields:**
- `name` (required) — skill identifier
- `description` (required) — one-line description, shown in system prompt
- `command` (optional) — slash command to register (e.g. `/reheat`)
- `script` (optional) — script to run when the command fires (path relative to skill dir)

**How it works:**
1. Config lists directories to scan: `[skills] dirs = ["/home/clod/skills"]`
2. On startup, scan each dir for subdirectories containing `SKILL.md`
3. Parse frontmatter, collect name + description into a registry
4. Inject skill list (name, description, SKILL.md path) as a system prompt block — the agent knows what's available but doesn't load full instructions until needed
5. The agent reads the full `SKILL.md` with the `read` tool when it decides a skill applies
6. If `command` + `script` are both present, auto-register as a slash command (runs the script directly, no agent turn)

Skills are not dynamic plugins — no code loading, no compilation. Just directories of files the agent can read, with optional shell scripts for slash commands.

### Memory System

**Alpha:** File-based with FTS5 search.
- Memory files in `workspace/memory/YYYY-MM-DD.md`
- Curated long-term memory in `workspace/MEMORY.md`
- `MEMORY.md` injected into system prompt on each turn

**Search:** SQLite FTS5 index over two sources, with explicit memories weighted higher:

1. **Explicit memories** (weight: 2.0) — all `.md` files in memory directory + `MEMORY.md`
2. **Conversation history** (weight: 1.0) — past messages from the conversation SQLite log

Both are indexed into the same FTS5 table with a `source` column to enable weighting:

```sql
CREATE VIRTUAL TABLE memory_fts USING fts5(
  content, path, source,    -- source: 'memory' or 'conversation'
  tokenize='porter unicode61'
);

-- Search with explicit memories ranked higher
SELECT path, snippet(memory_fts, 0, '→', '←', '...', 30),
       CASE source WHEN 'memory' THEN rank * 2.0 ELSE rank END AS weighted_rank
FROM memory_fts 
WHERE memory_fts MATCH ?
ORDER BY weighted_rank;
```

**Indexing:**
- Memory files: re-indexed on startup and when files change (fsnotify or periodic rescan)
- Conversation history: indexed as messages are logged (already going to SQLite)
- Incremental: only re-index changed files, not full rebuild each time

**Why FTS5 over vector embeddings:**
- Zero dependencies (built into SQLite, which we already use)
- Instant queries, no API calls
- Deterministic, debuggable
- Covers 90% of memory recall — you usually remember roughly what you wrote

**Maybe later:** Vector embeddings for semantic search when FTS5 misses. But not until FTS5 proves insufficient.

### Compaction

**Alpha:** Simple threshold-based.
- When context exceeds N% of model's context window, trigger compaction
- Pre-compaction: inject system message "save important context to memory now", let agent write to memory files
- Pre-session-end: same memory prompt fires before a session goes inactive — e.g. when the user runs `/new`, or after N minutes of inactivity. The agent gets a chance to persist anything important before the session is replaced or archived.
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

System crontab is sufficient — no built-in scheduler planned.

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

## Concurrency & Interrupts

Hard constraints learned from OpenClaw's failure modes. These aren't nice-to-haves.

### Message receiving never blocks

The Telegram listener runs on its own goroutine. It receives and queues messages regardless of what the agent is doing. Even if the agent is mid-way through a 5-minute tool call, incoming messages are received, logged, and — if they're slash commands — executed immediately.

```
[telegram goroutine]  →  receive msg  →  slash command?  →  yes: execute, reply
                                                          →  no:  enqueue for agent
[agent goroutine]     →  dequeue msg  →  build turn  →  call API  →  run tools  →  reply
```

Two goroutines, one channel. The agent pulls from the queue at its own pace. The receiver never waits on the agent.

### Agent turns are cancellable

Every agent turn gets a `context.Context`. When a cancel signal arrives (new `/stop` command, shutdown, timeout), the context is cancelled and:

- In-flight Anthropic API calls abort via the HTTP client's context
- In-flight tool executions (exec, web_fetch) abort via process kill
- The agent loop checks `ctx.Err()` between tool calls and after API responses

**Stop means stop, immediately.** Not "after the current tool finishes." If exec is running a 3-minute command and the user sends `/stop`, the process is killed within seconds. This is a first-class design constraint.

```go
func (a *Agent) RunTurn(ctx context.Context, msg string) error {
    // Every API call and tool execution passes ctx
    resp, err := a.client.Send(ctx, messages)
    if ctx.Err() != nil {
        return ctx.Err() // cancelled mid-API-call
    }
    for _, tool := range resp.ToolCalls {
        result, err := a.tools.Execute(ctx, tool)
        if ctx.Err() != nil {
            return ctx.Err() // cancelled mid-tool
        }
    }
}
```

### Long-running tools yield control

Tool executions that may block (exec with long commands, web_fetch on slow endpoints) must be interruptible via context cancellation. The exec tool runs commands in a child process and kills the process group on context cancel.

No tool call should prevent the system from responding to interrupts. If it does, that's a bug.

### Session reset guard

`/reset` refuses when the agent is mid-turn, preventing accidental data loss. This is the only reset mechanism — clod has no automatic daily/idle session resets. Sessions persist until explicitly reset by the user or the process restarts.

If automatic resets are added later: never reset an active session. A session is "active" if the agent is processing a turn OR the last message was received less than N minutes ago. OpenClaw's blunt `updatedAt < dailyResetAt` check wiped an active conversation mid-flow — that's the failure to avoid.

## Logging

Two log outputs, both plain files on disk. No systemd journal dependency.

### Event log (`clod.log`)
Human-readable, one line per event. Timestamp + level + component + message.

```
2026-02-21T03:52:39Z INFO  [telegram] bot started as @rk_clodbot
2026-02-21T03:52:41Z INFO  [agent] stop_reason=end_turn input=1119 output=164 cache_read=0 cache_write=1119
2026-02-21T03:53:01Z WARN  [exec] command timed out after 30s
2026-02-21T03:53:05Z ERROR [anthropic] 529 overloaded, retrying in 5s
```

Levels: DEBUG, INFO, WARN, ERROR. Default: INFO. Configurable in TOML.

Also writes to stderr so `tmux capture-pane` and `journalctl` (if run as a unit) work naturally.

### API log (`api.jsonl`)
Structured JSONL, one object per API request. For debugging cache behaviour, tracking costs, auditing usage.

```json
{"ts":"2026-02-21T03:52:41Z","session":"agent:main:main","model":"claude-haiku-4-5","input":1119,"output":164,"cache_read":0,"cache_write":1119,"cost_usd":0.003,"duration_ms":1240}
```

Searchable with `jq`. The agent can query its own API logs via tools.

**Full payload logging:** Optional — records complete API request/response bodies (system prompt, messages, tool calls, full response). Off by default (large files, contains conversation content). Enable in config:

```toml
[logging]
full_payload = true   # write full API payloads to api-payload.jsonl
```

Useful during development and debugging. The agent and `/last` can reference it for detailed inspection of what was actually sent to Anthropic.

### Cache bust alerts

When a single API call writes more than a configurable threshold of cache tokens, clod sends an immediate Telegram notification to the session's chat. This is a plain Telegram message, not an agent turn — zero tokens spent.

```toml
[logging]
cache_bust_detect = true  # alert when cache_read drops >50% vs previous request
```

```
⚠️ Cache write: 43,201 tokens ($0.27) on agent:main:main
```

Default threshold: 20,000 tokens. Set to 0 to disable. Helps catch system prompt mutations, unexpected session resets, or compaction failures that silently blow up costs.

## Slash Commands

Messages starting with `/` are intercepted before reaching the agent. They execute immediately - never queued behind an in-flight agent turn. This is a hard architectural constraint: commands must bypass the agent reply pipeline entirely.

### Built-in commands

**Session:**
- `/status` - session key, message count, total tokens (input/output/cache read/write), model, uptime, current agent turn status (idle/processing)
- `/reset` - clear session history, start fresh. Confirms before acting.
- `/model [name]` - show current model, or switch to `name` for this session
- `/session` - dump raw session metadata (message count, created at, last activity, compaction count)

**Debug & inspection:**
- `/cache` - last 5 API calls with cache hit/miss breakdown from api.jsonl. Shows: tokens in, cache read, cache write, cost. Quick way to verify caching is working.
- `/last` - show the last API request/response: model, stop reason, token usage, duration, cost. The single most useful debug command.
- `/tools` - list registered tools with enabled/disabled status
- `/config` - dump current running config (redact secrets)
- `/ping` - return "pong" with timestamp. Simplest possible liveness check.

**Logs:**
- `/log [n]` - last `n` lines from clod.log (default 20)
- `/errors [n]` - last `n` ERROR/WARN lines from clod.log (default 10)
- `/cost [today|session]` - total API cost from api.jsonl, grouped by session. Default: today.

**Context:**
- `/context` - character count breakdown of the full prompt. Shows each section separately: system prompt (per character file: IDENTITY.md, SOUL.md, etc.), tools schema, conversation history (user/assistant/tool messages), total. Helps diagnose what's eating context and whether cache is being used efficiently.

**System:**
- `/version` - binary version, go version, build time, git commit
- `/uptime` - process uptime, system load, memory usage

### Custom commands (TOML config)

```toml
[[commands]]
name = "usage"
description = "Show API usage stats"
script = "jq -s 'map(.cost_usd) | add' api.jsonl"

[[commands]]
name = "logs"
description = "Recent event log"
script = "tail -20 clod.log"

[[commands]]
name = "health"
description = "System health check"
script = "~/scripts/health-check.sh"
```

Each custom command runs a shell script and returns stdout as a Telegram message. Timeout: 10s default, configurable per command.

### Code-defined commands

Commands can also be registered in Go for anything that needs access to internal state:

```go
type Command struct {
    Name        string
    Description string
    Execute     func(ctx context.Context, args string) (string, error)
}
```

Built-in commands are code-defined. Custom commands from TOML are script-defined. Both share the same dispatch path.

### Dispatch

1. Telegram message arrives starting with `/`
2. Router matches command name (before any agent processing)
3. Execute immediately, return result to Telegram
4. Never touches the agent session or message history

If the agent is mid-turn processing a previous message, `/status` still returns instantly. That's the point.

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
port = 18791
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
- FTS5 memory search (memory files + conversation history)
- TOML config
- Wake endpoint for cron

**Out (beta):**
- Vector memory search (semantic, if FTS5 proves insufficient)
- Multi-agent support (design for it, don't build it)
- Sub-agents
- File attachments
- Skill framework
- Signal/Discord/other channels

**Out (enhancement):**
- Provider abstraction — pluggable backends for LLM (OpenAI, Gemini, local models via Ollama), STT (Groq Whisper, local Whisper, Google STT), TTS (Edge TTS, OpenAI TTS, Piper local, Google TTS). Currently hardcoded to Anthropic/Groq/Edge — abstract behind interfaces when a second provider is actually needed, not before.
- Per-session heartbeat configuration — different session types get different heartbeats. Main: general idle heartbeat (reads heartbeat.md). Fork/multiball: cache-aware heartbeat that fires N minutes before cache TTL expires ("Cache going cold, continue or wrap up?"). Subagents: no heartbeat. Configurable per session type in TOML.
- Telegram markdown/HTML rendering — convert agent markdown output to Telegram's MarkdownV2 or HTML format for proper bold, italic, code blocks, links. Currently sends plain text.
- Memory coverage tracking — SQLite log of memory writes with session_key, msg_id_start, msg_id_end, memory_type, file_path. No content duplication (content lives in messages table). Enables auditing what's been captured vs what's uncovered: join messages against memory_log to find gaps. Slash command `/memories` to show recent writes and coverage stats.
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

## Setup Script (`setup.sh`)

Idempotent. Run it once to install, run it again to update. Safe to re-run.

### What it does

1. **System user:** Create `clod` user if it doesn't exist (no login shell, home at `/home/clod`)
2. **Binary:** Build from source (`go build`) or download prebuilt release. Install `clod` and `clodgw` to `/usr/local/bin/`
3. **systemd service:** Install `/etc/systemd/system/clod.service` if it doesn't exist. `User=clod`, `WorkingDirectory=/home/clod`, restart on failure. Enable and start.
4. **Config:** Write `/home/clod/clod.toml` if it doesn't exist. Prompt interactively for:
   - Telegram bot token
   - Anthropic API token  
   - Telegram user ID (allowed_users)
   - Agent model (default: claude-haiku-4-5)
5. **Character files:** Create `~/character/` with template content if files don't exist:
   - `identity.md` — name, vibe, emoji
   - `soul.md` — inner life, what you notice
   - `user.md` — about your human
   - `agents.md` — how you work
   - `tools.md` — what you use
   - `memory.md` — what you've learned
6. **Directories:** Create `~/sessions/`, `~/workspace/memory/`, `~/character/` under clod's home
7. **Log rotation:** Install logrotate config for `clod.log` and `api.jsonl` (weekly, keep 4, compress)
8. **PATH:** Symlinks already in `/usr/local/bin/`, nothing extra needed

### Config references character files

```toml
[agent]
workspace = "/home/clod/character"
# bootstrap loads: identity.md, soul.md, user.md, agents.md, tools.md, memory.md
```

### Template content

Character file templates are minimal starters — just enough structure for the agent to understand what goes where, with placeholder text encouraging the human to fill them in. Not our files — generic ones.

### Update mode

When binaries already exist: rebuild/re-download, restart service. When config already exists: don't touch it. When character files already exist: don't touch them. Idempotent means safe.

### What it doesn't do

- No reverse proxy setup (that's deployment-specific)
- No DNS/domain config
- No external port exposure (binds to localhost by default)
- No automatic Telegram webhook (uses long-polling)

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
