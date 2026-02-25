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

- `agent:fotini:chat:123456789` — per-chat DM session (keyed by Telegram chat ID)
- `agent:main:cron:morning-routine` — cron-triggered branch
- `agent:main:subagent:research-task` — sub-agent branch
- `agent:main:multiball:mb-1709123456` — multiball fork

Each Telegram DM gets its own session, keyed by chat ID. One session is designated as the "default" — this is what heartbeats, cron (`clod send`/`clod branch`), and proactive features target. If no default is set, the first chat session created becomes the default. The default can be changed via `/sessions default <chat_id>`.

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
- Startup notification: sends "botname restarted at HH:MM:SS" to the last active chat. Controlled by global `enable_startup_notify` (default true) with per-agent override via `startup_notification`. Set to `false` for silent bots (e.g., cron-only agents).

### Multi-Bot Sessions (/multiball)

An agent can have multiple Telegram bots assigned — one primary, the rest secondary. Secondary bots are idle until needed. Bots can be assigned per-agent or to a shared pool that any agent can use as fallback.

```toml
[[agents]]
id = "clutch"
telegram_bot = "primary"
multiball_bots = ["clutchling", "clutchling2"]  # per-agent pool
allowed_users = ["5970082313", "1234567"]       # only these users (overrides global)

[[agents]]
id = "research"
telegram_bot = "secondary"
# no allowed_users — falls back to global [telegram] allowed_users

[telegram]
allowed_users = ["5970082313"]                   # global default
multiball_bots = ["spare1", "spare2"]            # shared pool (fallback for any agent)
```

**Acquisition priority:** per-agent pool first, shared pool as fallback. Released bots return to whichever pool they came from.

**`/multiball` (alias `/mb`):**
1. Fork the current session (cache-sharing branch)
2. Acquire the least-recently-used bot (per-agent pool first, then shared pool)
3. That bot sends the user a Telegram message: "🎱 Forked from main. What do you need?" (plain Telegram message, not an agent turn — no tokens spent)
4. All subsequent messages to that bot route to the forked session

The user now has two (or more) parallel conversations with the same agent, each in its own Telegram chat, sharing the cached prefix. When the fork is done, `/done` in that chat detaches the bot and returns it to the pool.

**Restart survival:** The `bot → session_key` mapping is persisted in the state store (`multiball:<telegram_username>` → `<session_key>`). On restart, mappings are restored if the session file still exists on disk. Stale mappings (session file deleted) are cleaned up automatically.

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

### Image Persistence

When `image_save_dir` is configured (global or per-agent), received images are saved to disk with a timestamped filename (`YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext`). The saved path is injected into the user message text as `[Image saved to: /path/to/file.jpg]` so the agent can reference, copy, or process the file. Saving is non-fatal — errors are logged as warnings and the image is still sent to the API as usual.

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

Stored in SQLite, scoped per-agent via `agent_id` column. On compaction, scratchpad contents are injected back into the post-compaction context as a system message. The agent is responsible for clearing it when done — it's working state, not knowledge.

### Spawn (Model Escalation + Self-Fork)

The `spawn` tool is a unified sub-call mechanism with three context modes:

```
spawn(prompt="Evaluate this architecture", model="opus", context="none")
spawn(prompt="Research this topic thoroughly", context="clone_current")
```

**Context modes:**

- **`none`** — just the prompt, no system context, no tools. One-shot cold call. Cheapest option for simple questions.
- **`full`** — character files + prompt, no tools. One-shot call with full personality context. Good for tasks that need "you" without tool access.
- **`clone_current`** (default) — creates a branch session with full tool access. A headless self-fork: the spawned session inherits the parent's context, tools, and model. Always runs asynchronously — returns an immediate acknowledgment and delivers the result via `AsyncNotifier` when complete.

**Inherit mode details:**
- Creates a branch session: `agent:AGENTID:spawn:spawn-TIMESTAMP`
- Branch has `NoResetHook` set (ephemeral, no memory formation on cleanup)
- Recursive clone_current spawns are blocked — a spawned session can use `none`/`character_only` but not `clone_current`
- Concurrent clone_current spawns are limited by `max_concurrent_spawns` (default 3)
- Runs as a full agent turn with all tools available
- Always async: returns `"Spawn started in background."` immediately, delivers `[SPAWN RESULT]` via notifier on completion (matching the `[EXEC RESULT]`/`[HTTP RESULT]` pattern)

**Model resolution:** Short names (`opus`, `sonnet`, `haiku`) resolve to full model IDs. Empty model defaults to the parent's model. Model is ignored for clone_current mode (inherits parent model).

### Thought Queue

The agent can defer thoughts for later via a `memory_remind` tool:

```
memory_remind("Look into whether FTS5 supports phrase boosting", "next_heartbeat")
memory_remind("Ask Dick about the Greece decision", "tomorrow")
```

Reminders surface as injected context at the specified time (next heartbeat, next session, specific date). Stored in SQLite, scoped per-agent via `agent_id` column. Lightweight — not a full task system, just "future me should think about this."

### Tool System

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery.

**Alpha tools:**
- `exec` — run shell commands (with timeout, background, auto-background)
- `tmux` — manage tmux sessions (start, send keys, read pane output, list, kill)
- `read` — read file contents
- `write` — create/overwrite files
- `edit` — find-and-replace in files
- `web_fetch` — HTTP GET, extract readable content
- `web_search` — Brave Search API
- `memory_search` — FTS5 search over memory files + conversation history
- `memory_remind` — defer a thought for later (next heartbeat, tomorrow, specific date)
- `spawn` — sub-call to a model (none/character_only: one-shot, clone_current: branch session with full tools)
- `send_to_session` — inject a message into another session (cross-session communication). `reply_to` param: `"caller"` (default) routes response back to calling session, `"session"` sends response to the target session's own Telegram chat
- `schedule_wake` — schedule a message to be sent to the session at a specific time or delay
- `tts` — convert text to speech via TTS provider (OpenRouter, Edge TTS)
- `todo` — manage a per-agent task list (add, list, complete, remove) with priority ordering
- `bitwarden_search` — search Bitwarden vault items by name/URI/folder (metadata only, no passwords)
- `bitwarden_unlock` — unlock a vault item (requires admin approval via aisudo/Telegram), caches for TTL

### Tmux Session Monitoring

The `tmux` tool includes operations for monitoring pane inactivity:

- `watch` — monitor a pane for inactivity; fires if content unchanged for threshold seconds (default 30s)
  - Parameters: `session` (required), `window` (default 0), `threshold_seconds` (default 30)
  - Tracks content with MD5 hash; timer resets on change
  - Runs as background goroutine, one-shot alert mechanism
- `unwatch` — stop monitoring a session

**Use case:** Long-running background tasks. Start a build/deploy with `tmux start`, then `watch` to be notified when it completes.

### Tmux Memory Monitor

Background goroutine checking the tmux server's RSS at configurable intervals. Long-running tmux sessions (especially hosting TUI apps) accumulate memory via glibc malloc fragmentation.

Three thresholds (all configurable as `%` of RAM, `mb`, or `gb`):
- **warn** (default 10%) — log WARN, send Telegram notification
- **critical** (default 20%) — log WARN (stronger), send Telegram notification
- **kill** (default 30%) — log ERROR, send Telegram notification, `tmux kill-server`, clean up tool state

Notifications go to agents whose `inject_agent_warnings` is false. Dedup prevents spam: same threshold only fires once until memory drops below it or tmux is killed.

### Tool Result Guard

When a tool returns a result exceeding a configurable character threshold (default: 10,000 chars), clod does NOT inject the full result into session history. Instead:

1. Write the full result to a temp file: `{temp_dir}/tool-result-{tool}-{random}.txt`
2. Return a truncated result to the agent with instructions:
   ```
   [Result too large: 47,231 chars. Full output saved to /tmp/clod-tool-results/tool-result-exec-a1b2c3d4.txt]
   Use `read` tool to inspect sections. First 10000 chars:
   ...
   ```

This prevents large tool results (e.g. `exec cat bigfile.txt`) from permanently bloating session history. The agent can still access the full result via `read` — it just doesn't sit in context forever.

```toml
[tools]
max_result_chars = 10000              # max chars before writing to file
temp_dir = "/tmp/clod-tool-results"   # where to write large results
```

**http_request — file saves, binary handling, and auto-background:**
- `save_to` — save response body to a specific file path (returns status + headers + path, not body)
- `save_from_json_path` — extract a value from JSON response by dot path (e.g. `data.0.url`); if it's a `data:` URI, decodes base64 to binary. Requires `save_to`. Designed for image generation APIs that return base64 data URIs.
- Binary content types (`image/*`, `audio/*`, `video/*`, etc.) auto-save to temp file when `save_to` is not set
- `background` parameter — if `true`, request runs immediately in background and result is delivered asynchronously
- Auto-background — if a request exceeds the `exec_auto_background` threshold, it auto-backgrounds and the result is delivered when complete (same mechanism as exec)

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

**Alpha:** File-based with FTS5 search and multiple weighted sources.
- Memory files in `workspace/memory/YYYY-MM-DD.md`
- Curated long-term memory in `workspace/MEMORY.md`
- `MEMORY.md` injected into system prompt on each turn

**Multiple Sources with Weights:**

Configure multiple indexed directories, each with a configurable weight multiplier in `clod.toml`:

```toml
[[memory.sources]]
name = "canonical"
dir = "/home/clod/workspace/memory"
weight = 1.0      # highest priority: 2.0x multiplier

[[memory.sources]]
name = "code"
dir = "/home/clod/src"
weight = 0.3      # lower priority: 1.3x multiplier

[[memory.sources]]
name = "docs"
dir = "/home/clod/docs"
weight = 0.5      # medium priority: 1.5x multiplier
```

Each source is indexed with `source={sourceName}` and searched with weight multiplier: `rank * (1.0 + weight)`.

**Backward Compatibility:**

If `sources` array is empty, fall back to single `dir` field (default weight=1.0):

```toml
[memory]
dir = "/home/clod/workspace/memory"   # old way, still works
```

**Search:** SQLite FTS5 index over multiple sources with conversation history:

```sql
CREATE VIRTUAL TABLE memory_fts USING fts5(
  content, path, source,    -- source: 'canonical'|'code'|'docs'|'conversation'
  tokenize='porter unicode61'
);

-- Search with per-source weights
SELECT path, snippet(memory_fts, 0, '→', '←', '...', 30),
       CASE source
         WHEN 'canonical' THEN rank * 2.0    -- (1.0 + 1.0)
         WHEN 'code' THEN rank * 1.3         -- (1.0 + 0.3)
         WHEN 'docs' THEN rank * 1.5         -- (1.0 + 0.5)
         WHEN 'conversation' THEN rank * 1.0 -- default
       END AS weighted_rank
FROM memory_fts
WHERE memory_fts MATCH ?
ORDER BY weighted_rank;
```

**Indexing and Auto-Reindex:**

- Memory files: re-indexed on startup
- File watching: optional auto-reindex when `.md` files change via fsnotify
- Debounce: configurable delay (default 0s = immediate):

```toml
[memory]
reindex_debounce = "500ms"   # wait 500ms after file change before reindexing
```

- Conversation history: indexed as messages are logged (already going to SQLite)

**Why FTS5 over vector embeddings:**
- Zero dependencies (built into SQLite, which we already use)
- Instant queries, no API calls
- Deterministic, debuggable
- Covers 90% of memory recall — you usually remember roughly what you wrote

**Maybe later:** Vector embeddings for semantic search when FTS5 misses. But not until FTS5 proves insufficient.

### Compaction

**Alpha:** Threshold-based with fully configurable parameters.
- When context exceeds N% of model's context window, trigger compaction
- Pre-compaction: inject system message "save important context to memory now", let agent write to memory files
- Pre-session-end: same memory prompt fires before a session goes inactive — e.g. when the user runs `/new`, or after N minutes of inactivity. The agent gets a chance to persist anything important before the session is replaced or archived.
- Compaction: call model with configurable summary prompt, replace history with summary
- Post-compaction: inject handoff note so agent knows compaction occurred
- Scratchpad preserved through compaction (appended to handoff)

**Configuration:**
```toml
[sessions]
compaction_threshold = 0.8               # compact at 80% of context window
compaction_max_tokens = 4096             # max output tokens for summary
compaction_min_messages = 4              # min messages before compacting
compaction_summary_prompt = ""           # path to summary prompt file (empty = minimal fallback)
compaction_system_prompt = ""            # path to extra system prompt for compaction (empty = disabled)
compaction_handoff_msg = "..."           # message after compaction
compaction_debug = false                 # send summary as Telegram file attachment (default false)
session_reset_prompt = ""                # path to reset prompt file (empty = disabled)
```

All parameters have sensible defaults. Customize only what you need. Prompt files are read live at the point of use — edits take effect immediately without restart or `/reload`.

### Heartbeat

A timer that fires when the session has been idle for a configurable duration.

- Injects a heartbeat message into the session
- Agent processes it like any other turn (reads HEARTBEAT.md, decides what to do)
- If agent responds with `HEARTBEAT_OK`, no action taken
- Configurable interval (default: 45 minutes)

### Scheduled Wakes

**HTTP endpoint (for cron jobs):**
```
POST /wake
{"agent": "main", "text": "morning routine", "no_compact": true}
```
Injects text as a user message into a branch session. When `no_compact` is true, the session returns its result instead of triggering compaction if the context limit is reached — useful for cron jobs that inherit a large parent context and shouldn't waste mana compacting.

**Tool-based scheduling:**
The `schedule_wake` tool allows the agent to schedule messages to itself:
- `delay: "30m"` — schedule message after a duration (e.g., "30m", "2h", "1d")
- `at: "2026-02-21T15:30:00Z"` — schedule message at ISO timestamp
- One-shot, auto-cleaned after firing
- Useful for self-reminders, follow-ups, or timed actions

System crontab can trigger `/wake` endpoint for external scheduling. For agent-initiated delays, use the `schedule_wake` tool.

### Activity gating

Both `POST /send` and `POST /wake` accept an optional `if_active` field (Go duration string, e.g. `"8h"`). When set, the request is silently skipped (HTTP 200 with `"skipped: no recent user activity"`) if no real Telegram user has messaged the agent within the window.

"Real user activity" means messages from allowed Telegram users via the primary bot. It explicitly excludes: CLI-injected messages (`clod send`/`clod branch`), heartbeats, async notifications, agent-to-agent messages, and system-injected messages. This prevents the gate from defeating itself — a heartbeat send cannot reset the activity timer.

The timestamp is stored per-agent in the state store (`agent:<id>:last_user_activity`). The CLI exposes this as `--if-active <duration>` on `send` and `branch` commands. See [docs/CLI.md](docs/CLI.md) for full CLI reference.

### Secrets

Secrets never pass through agent context. The agent cannot read, echo, or exfiltrate credentials.

### Principle
Credentials are loaded once at startup into process memory. Built-in integrations (Anthropic, Telegram, Brave Search) use them directly from Go structs. The agent interacts with tools, tools use credentials internally — the agent never constructs auth headers or sees token values.

### Architecture

**`secrets.toml`** — separate from main config, protected by OS-level group permissions. See [docs/SECRETS.md](docs/SECRETS.md) for full details.

**OS-level protection (primary):**

- `secrets.toml` owned by `root:clod-secrets`, mode `0660`
- Main clod process has `clod-secrets` as a supplementary group (via systemd `SupplementaryGroups`)
- All child processes (exec tool, tmux tool, script commands) have supplementary groups dropped — they run with only the primary `clod` group
- The OS kernel denies access regardless of how the path is specified (encoding tricks, globs, interpreter string construction all fail)
- Requires `AmbientCapabilities=CAP_SETGID` in the systemd unit for `setgroups()` to work

**Defence-in-depth layers:**

1. **Built-in integrations** — Anthropic client, Telegram bot, etc. receive credentials via Go structs at init. Agent calls tools; tools use credentials internally. Zero exposure.

2. **Exec template references** — For ad-hoc commands the agent can reference secrets by name:
   ```
   curl -H "Authorization: Bearer {{secret:custom.github_token}}" https://api.github.com/...
   ```
   Clod resolves `{{secret:NAME}}` before spawning the subprocess. The agent sees the template, never the value. Unresolved references are an error (not silently passed through).

3. **Output redaction** — Exec tool output is scanned for known secret patterns and redacted before returning to the agent. Catches accidental leaks from `env`, error messages, config dumps, etc.

4. **Blocked paths** — The exec tool refuses to read `secrets.toml`, `/proc/self/environ`, and any path matching a configurable blocklist. String-match check as additional layer.

5. **Startup security checks** — At startup, verifies file ownership, permissions, and group membership. Warns if misconfigured (does not block startup). Disable with `skip_security_checks = true`.

**Bitwarden vault integration (optional):**

A dynamic secret store backed by the Bitwarden CLI, with a two-tier aisudo approval model:
- **Metadata (list)** — `sudo -u bitwarden bw list items` is allowlisted in aisudo, runs without approval. Caches item names, URIs, folders, usernames. Refreshed on a configurable interval.
- **Passwords (get)** — `sudo -u bitwarden bw get password <id>` requires Telegram approval via aisudo. Blocks until approved or denied. Cached with configurable TTL.
- **Template syntax** — `{{secret:bw.ITEM_UUID}}` in http_request headers/body. Host validation uses the vault item's URI fields.
- **Dedicated system user** — `bitwarden` user owns the CLI session state and session file. Not root. Clod never reads the session token — each `bw` command reads the session file as the bitwarden user.

**Per-agent secrets:**

Secrets in `secrets.toml` are global by default. Agents can have their own overrides via `[agents.ID]` sections:
```toml
[custom]
github_token = "ghp_default"

[agents.fotini.custom]
github_token = "ghp_fotini_account"
```
Resolution order: agent-specific value wins over global. Keys not overridden in the agent section fall back to globals. Each agent only sees its own overrides — agent A cannot see agent B's secrets. Built-in credential resolution (anthropic.token, telegram, brave) stays global (process-wide); per-agent scoping applies to tool-visible secrets (exec templates, http_request, redaction, system prompt secret names).

### What the agent knows
- That secrets exist (by name): "anthropic", "telegram", "brave", "custom.github_token"
  - Available secret names are injected into the system prompt at startup so the agent can discover what's available
  - Per-agent overrides add or replace names visible to that agent
  - Unresolved secret references in exec commands are errors (not silently passed through)
- If bitwarden is enabled, the agent knows it can search the vault and request unlocks
  - The agent never sees password values — only template references `{{secret:bw.ID}}`
- How to reference them: `{{secret:NAME}}` (static) or `{{secret:bw.UUID}}` (bitwarden)
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

**Pre-reset memory hook:** Before clearing the session, if `session_reset_prompt` is configured, the agent gets one final turn to save important context to memory files. The hook has a 60-second timeout and is non-fatal — if it fails, the reset proceeds. Branch sessions can opt out via `NoResetHook` in their branch metadata (set via `--no-reset-hook` or `--oneshot` CLI flags). The same hook fires on multiball TTL reclaim.

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
cache_bust_detect = true   # alert when cache_read drops >50% vs previous request
cache_bust_idle_minutes = 10  # suppress alert if session idle > N minutes (cache expired naturally)
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
- `/usage` - check Claude subscription usage and rate limits (requires OAuth token in config)
- `/tools` - list registered tools with enabled/disabled status
- `/config` - show usage. `/config toml` for raw TOML output. `/config table` for formatted config table. `/config available` to discover unset options.
- `/ping` - return "pong" with timestamp. Simplest possible liveness check.
- `/prompts` - show configured prompt paths (compaction summary, session reset, handoff message, fork prompt) with existence checks, plus prompt files found on disk in workspace and shared directories.

**Logs:**
- `/log [n]` - last `n` lines from clod.log (default 20)
- `/errors [n]` - last `n` ERROR/WARN lines from clod.log (default 10)
- `/cost <subcommand>` - API cost from api.jsonl. No args: show usage. `today`: per-session table. `24h`: rolling 24h with per-category table. `week`: 7-day daily table. `<days>`: total for last N days.

**Context:**
- `/context` - full context window breakdown. Shows: total tokens vs model max with percentage and compaction threshold; system prompt section-by-section (each workspace file, environment block, skills block) with character counts; conversation breakdown by role (user, assistant, tool results) with message counts; last API call token details (input, cache_read, cache_write, output).

**Sessions:**
- `/sessions` or `/sessions list` — list all per-chat sessions for this agent. Shows chat ID, username, message count, last active time, and which is the default (★).
- `/sessions default <chat_id>` — set a specific chat as the default session (used by heartbeats, cron, proactive features).
- `/sessions info` — show details for the current chat's session (chat ID, default status, message count, username).

**Agents:**
- `/agents` - list active agent sessions with status, model, and message counts
- `/agents new` - interactive wizard for creating a new agent. Walks through: agent ID, display name, emoji, model, bot token secret, character file mode. Creates workspace, appends config to clod.toml, adds crontab entries. Requires restart to activate.

**System:**
- `/version` - binary version, go version, build time, git commit
- `/uptime` - process uptime, system load, memory usage
- `/reload` - reload config and workspace files (IDENTITY.md, SOUL.md, etc.) without restarting

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

**Single agent (legacy):**
```toml
# clod.toml

[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/home/rich/git/openclaw/workspace"
heartbeat_interval = "45m"

[anthropic]
token = "sk-ant-oat01-..."
oauth_token = "sk-ant-oat01-..."  # OAuth token for /usage command

[telegram]
bot_token = "8351531463:AAH..."
allowed_users = ["5970082313"]

[sessions]
dir = "/home/rich/git/clod/sessions"
compaction_threshold = 0.8
compaction_max_tokens = 4096
compaction_min_messages = 4

[memory]
dir = "/home/rich/git/openclaw/workspace/memory"

[tools]
max_result_chars = 10000
temp_dir = "/tmp/clod-tool-results"

[voice]
stt_endpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
stt_model = "whisper-large-v3"
tts_provider = "edge-tts"

[http]
port = 18791
bind = "127.0.0.1"

[logging]
level = "INFO"
event_file = "clod.log"
api_file = "api.jsonl"
```

**Multi-agent:**
```toml
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/home/rich/workspace1"
telegram_bot = "primary"              # references [telegram.bots.primary]
multiball_bots = ["clutchling"]       # per-agent multiball pool

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"
telegram_bot = "scout"
# no multiball_bots = uses shared pool only (if configured)

[telegram]
allowed_users = ["5970082313"]
multiball_bots = ["spare1"]           # shared pool (fallback for any agent)

[telegram.bots]
primary = { token_secret = "telegram.primary" }
clutchling = { token_secret = "telegram.clutchling" }
scout = { token_secret = "telegram.scout" }
spare1 = { token_secret = "telegram.spare1" }
```

Both formats supported. `[agent]` (singular) is auto-promoted to a single-element `[[agents]]` array. Bot tokens resolved from `secrets.toml` via `token_secret` reference.

## Implementation Status

### ✅ Alpha — Complete

- Anthropic API client with prompt caching (auto strategy, deterministic tool ordering)
- Session store (JSONL-backed)
- Session branching with cache sharing (confirmed: branches read cached prefix, zero cold starts)
- Multiball — `/multiball` forks to secondary Telegram bot, tested and working
- Wake/cron sessions — `POST /wake` creates branch sessions for cron jobs
- Telegram bot (text messages, DM only)
- Tools: exec, read, write, edit, web_fetch, web_search, http_request, memory_search, memory_remind, scratchpad (read/write/clear), send_telegram, send_to_session, tmux (watch/unwatch), spawn (none/character_only/clone_current), schedule_wake, tts, todo
- Workspace bootstrap (markdown files → system prompt, configurable file order)
- Skills framework (YAML frontmatter, command dispatch, script execution)
- Heartbeat (configurable interval)
- Compaction (threshold-based, configurable parameters, optional Telegram notification)
- FTS5 memory search (memory files + conversation history, weighted)
- TOML config (single file + secrets.toml)
- Voice outbound (Edge TTS or OpenAI, configurable speech rate via `tts_rate`)
- Voice inbound (STT via Groq Whisper)
- Deferred replies (multiple Telegram messages per turn)
- Cache bust alerts (Telegram notification on large cache writes)
- Regular secret templates blocked in exec — `{{secret:NAME}}` returns error, must use http_request. Bitwarden `{{secret:bw.*}}` still allowed (approval-gated)
- Secret redaction on all tool output — exec output, tool errors, and all tool results scanned for known secret patterns
- Telegram markdown rendering (HTML parse mode for rich formatting without escaping complexity)
- Tool result size guard (large results saved to temp file)
- Slash commands: /status, /cache, /ping, /last, /usage, /reload, /tools, /config, /model, /reset, /multiball, /sessions
- Cron system (system crontab, prompts loaded from disk)
- Setup script (idempotent, builds from source, installs as systemd service)
- Repair interrupted tool calls on session load
- Restart markers — on startup, appends `[System restarted]` to recently active sessions (within 1 hour)
- Gap calculation seeded from session history on restart (correct `gap=` on first post-restart message)
- Environment block — system-generated runtime context injected as first system prompt block (`[environment]` config)
- BotFather slash command registration on startup (Telegram `setMyCommands`)
- Todo tool — native todo management with SQLite persistence, per-agent scoping, priority ordering
- Per-agent scratchpad and reminders — `agent_id` column in shared databases, schema migration
- Async exec/tmux result routing — per-notification session key (results route to correct session, not hardcoded main)
- max_tokens warning — log WARN + Telegram notification when stop_reason=max_tokens
- Rate limit handling — API 429/529 errors detected as `*APIError`, friendly Telegram notification sent via `RateLimitFunc` callback (with estimated retry time from `Retry-After` header), clean error returned instead of raw API error
- Tool call errors logged as WARNING in event log
- Tool call visibility gating — `show_tool_calls` config (global + per-agent) controls whether tool call messages appear in Telegram. Default true (current behavior). Set false for user-facing agents where tool visibility is confusing

### 🔶 Phase 2 — Next

- **Inter-agent messaging** — agents communicating with each other
- **File attachments** — send/receive files via Telegram
- **Compaction pre-save prompt** — "save important context" before compacting
- **Pre-session-end memory prompt** — memory save on /new or idle timeout

### 🔷 Enhancement — Later

- Provider abstraction — pluggable backends for LLM (OpenAI, Gemini, local models via Ollama), STT (Groq Whisper, local Whisper, Google STT), TTS (Edge TTS, OpenAI TTS, Piper local, Google TTS). Currently hardcoded to Anthropic/Groq/Edge — abstract behind interfaces when a second provider is actually needed, not before.
- Per-session heartbeat configuration — different session types get different heartbeats. Main: general idle heartbeat (reads heartbeat.md). Fork/multiball: cache-aware heartbeat that fires N minutes before cache TTL expires ("Cache going cold, continue or wrap up?"). Subagents: no heartbeat. Configurable per session type in TOML.
- Telegram MarkdownV2/HTML rendering — upgrade from basic Markdown to MarkdownV2 or HTML for better formatting fidelity.
- Memory coverage tracking — SQLite log of memory writes with session_key, msg_id_start, msg_id_end, memory_type, file_path. No content duplication (content lives in messages table). Enables auditing what's been captured vs what's uncovered: join messages against memory_log to find gaps. Slash command `/memories` to show recent writes and coverage stats.
- Signal/Discord/other channels
- Plugin/hook architecture
- Reactions
- Config schema validation beyond basic TOML parsing

## Testing Priority

**✅ PASSED: Session branching cache sharing.**
Confirmed working — wake/cron branches read ~63k cached tokens on first request with zero cold starts. Multiball branches also share parent cache prefix.
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
