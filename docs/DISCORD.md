# Discord — Setup & Features

Discord support for Foci. One WebSocket gateway per bot token, Markdown pass-through, thread-based facets.

## Quick Start

### 1. Create a Discord Application

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
2. Click **New Application**, give it a name
3. Go to **Bot** in the left sidebar
4. Click **Reset Token** and copy the bot token
5. Under **Privileged Gateway Intents**, enable:
   - **Message Content Intent** (required — the bot reads message text)
   - **Server Members Intent** (optional — only if you need member info)

### 2. Invite the Bot to Your Server

Go to **OAuth2 → URL Generator**:
- **Scopes:** `bot`, `applications.commands`
- **Bot Permissions:** `Send Messages`, `Send Messages in Threads`, `Create Public Threads`, `Create Private Threads`, `Manage Threads`, `Embed Links`, `Attach Files`, `Read Message History`, `Use Slash Commands`, `Add Reactions`

Copy the generated URL and open it in your browser to invite the bot.

### 3. Get Your Discord User ID

Enable **Developer Mode** in Discord (Settings → Advanced → Developer Mode). Right-click your name in any chat and click **Copy User ID**.

### 4. Configure Foci

Add to `foci.toml`:

```toml
[discord]
allowed_users = ["YOUR_DISCORD_USER_ID"]
```

Add to `secrets.toml`:

```toml
[discord]
myagent = "MTIzNDU2Nzg5MDEyMzQ1Njc4OQ.GrMN5z.abc123..."
```

The bot name defaults to the agent ID. If your agent is `[[agents]] id = "myagent"`, the token is resolved from `discord.myagent` in secrets.

To use a different secret key:

```toml
[agents.platforms.discord]
bot = "mybot"                    # resolves from discord.mybot
bot_secret = "custom.secret.key" # or use a custom key entirely
```

### 5. Interactive Setup (Alternative)

```bash
foci setup
```

The setup wizard includes a Discord step that prompts for bot token and user ID, then writes the config. Auto-detect mode connects the bot and waits for you to send it a DM to capture your user ID.

---

## How It Works

### Gateway

Discord uses a single persistent WebSocket connection (via [discordgo](https://github.com/bwmarrin/discordgo)) instead of Telegram's HTTP long-polling. One connection handles all agents sharing the same bot token. The gateway registers handlers for `MessageCreate` (text messages, attachments) and `InteractionCreate` (button presses, slash commands).

Required intents: `GuildMessages`, `DirectMessages`, `MessageContent`, `GuildMessageReactions`.

### Message Flow

```
Discord gateway
  → onMessageCreate
    → guild restriction check (if guild_id set)
    → mention requirement check (if require_mention=true, guild channels only)
    → auth check (allowed_users)
    → strip bot mentions from text
    → download attachments (images, PDFs, docs)
    → intercept: wizard → last-msg → command dispatch → steer
    → enqueue to agent worker

Agent worker (sequential per bot):
  → dequeue message
  → start typing indicator (refreshed every 8s)
  → route to agent (HandleMessage / HandleMessageWithAttachments)
  → finalize response (stream edit or new message)
  → drain buffered notifications
  → drain steer messages (follow-up turns)
```

### Message Limits

Discord has a 2000-character message limit (vs Telegram's 4096). Long responses are split automatically:

- Prefers splitting at newline boundaries
- Falls back to space boundaries, then hard split
- Code fences (```` ``` ````) are closed before the split and reopened after — each chunk is valid Markdown

### Formatting

Discord speaks Markdown natively. Agent output passes through without conversion (unlike Telegram, which requires HTML transformation). Bold, italic, code blocks, links, lists — all work as-is.

---

## Commands

Commands work two ways in Discord:

### Dot Commands

Type `.command args` in chat. The leading `.` is stripped and routed to the command registry:

```
.model haiku       → switches model
.status            → shows agent status
.cost              → shows session cost
```

### Slash Commands

Type `/command args` in chat (text-based, not Discord's application slash commands). Same routing as dot commands:

```
/model haiku
/status
/sessions
```

Except `/stop` and `/done`, which are handled locally by the bot (never reach the command registry).

### Local Commands

| Command | Effect |
|---------|--------|
| `/stop` | Cancel the current agent turn. Propagates to in-flight API calls and tool executions. |
| `/done` | Detach a facet thread's secondary bot, returning it to the pool. On primary bots, replies "Nothing to detach." |

### Button Callbacks

When a command returns keyboard options (e.g., `/model` lists available models), Discord renders them as interactive buttons. Clicking a button dispatches the command with the selected argument. Chained keyboards (drill-down menus) are supported.

---

## Streaming

When `stream_output = true`, model output appears in Discord in real-time as tokens arrive.

- **Edit interval:** 1200ms by default (configurable via `stream_update_interval`). Discord's rate limits are stricter than Telegram's, so this is significantly slower than Telegram's 250ms default.
- **Max per edit:** 1900 characters (leaves headroom under the 2000-char limit).
- **Lazy start:** No message sent until the first text delta arrives. Pure tool-call turns produce no stream message.
- **Finalization:** If the final response fits in the stream message, it's edited in-place. If too long, the stream message becomes a preview and the full response follows as new message(s).

Config:

```toml
# Global
[discord]
stream_output = true
stream_update_interval = "1200ms"

# Per-agent override
[agents.platforms.discord]
stream_output = true
stream_interval = "1500ms"
```

---

## Tool Call Display

Controlled by `show_tool_calls`:

| Mode | Behavior |
|------|----------|
| `"off"` | Tool calls are silent. Only the final response is shown. |
| `"preview"` | Each tool call is sent as a message showing the tool name and parameters. The message is deleted when the response arrives. |
| `"full"` | Each tool call is sent as a compact summary with a **Show full** button. Clicking expands to show the full parameters and result. **Hide** collapses it back. |

Tool call summaries use prefix icons:

| Tool | Prefix |
|------|--------|
| `shell` | `> ` |
| `web_fetch`, `http_request` | `>> ` |
| `web_search`, `memory_search` | `?? ` |
| `read` | `[] ` |
| `write` | `<> ` |
| `edit` | `/\ ` |
| `tmux` | `:: ` |
| `todo` | `-- ` |
| `spawn` | `++ ` |
| `scratchpad` | `// ` |
| `remind` | `.. ` |

---

## Thinking Display

Controlled by `show_thinking`:

| Mode | Behavior |
|------|----------|
| `"off"` | Thinking blocks are hidden. |
| `"compact"` | Response is shown with a **Show thinking** button. Clicking reveals the thinking text. |
| `"true"` | Thinking is shown inline (italic) above the response, separated by a divider. |

---

## Facet Threads

Discord facets use **threads** instead of separate bot tokens (Telegram's approach). When `/facet` is called with `auto_thread = true`, a new thread is created in the agent's primary channel. The thread becomes the facet session's channel.

### Configuration

```toml
[discord]
auto_thread = true              # create threads for facets (default: true)
facet_session_ttl = "60m"       # idle TTL before thread is reclaimable
```

### Lifecycle

1. `/facet` → fork session → create thread → secondary bot attached
2. Messages in the thread are routed to the forked session
3. After `facet_session_ttl` idle time, the thread is reclaimable
4. `/done` in the thread detaches the bot, returning it to the pool
5. Thread sessions survive service restarts (restored from session index)

### Bot Pool

Facet bots can be per-agent or shared across agents. Acquisition is LRU (least recently used idle bot). When all bots are in use, the fork request fails immediately.

---

## Attachments

Discord attachments are downloaded directly from Discord's CDN (no intermediate file API like Telegram).

Supported types:
- **Images** (JPEG, PNG, GIF, WebP) → sent to agent as image content blocks
- **PDFs** → sent as document content blocks
- **Convertible documents** → text extraction pipeline
- **Other files** → saved to disk with annotation

Downloads retry 3x with exponential backoff (1s, 2s). 4xx errors fail immediately.

If `received_files_dir` is configured, attachments are saved to disk with timestamped filenames: `2026-03-16T20-15-30Z_attachment.pdf`.

---

## Mention & Guild Filtering

### Require Mention

In guild channels, the bot ignores messages that don't `@mention` it (default: on). DMs are always processed.

```toml
[discord]
require_mention = true    # default

# Per-agent override:
[agents.platforms.discord]
require_mention = false   # respond to all messages in guild channels
```

Bot mentions (`<@BOT_ID>` and `<@!BOT_ID>`) are stripped from the message text before processing.

### Guild Restriction

Lock the bot to a single Discord server:

```toml
[discord]
guild_id = "123456789012345678"
```

Messages from other guilds are silently ignored.

---

## Display Settings Cascade

Display settings resolve in this order (first non-nil wins):

1. **Per-session override** — set at runtime via `/display` command
2. **Per-agent platform config** — `[agents.platforms.discord]`
3. **Agent config** — `[[agents]]` top-level fields
4. **Global Discord config** — `[discord]`
5. **Hardcoded defaults**

```toml
# Global defaults
[discord]
show_tool_calls = "off"
display_width = 60

# Agent overrides
[[agents]]
id = "verbose-agent"

[agents.platforms.discord]
show_tool_calls = "full"
show_thinking = "compact"
display_width = 80
```

---

## Notifications

### Startup Notification

When `startup_notify = true` (default), the bot sends a startup message to each agent's default channel on service restart.

### Notification Buffering

During an active agent turn, notifications (compaction alerts, cost warnings, etc.) are buffered and delivered after the turn completes. Time-sensitive notifications (rate limit alerts) bypass the buffer.

---

## Session Keys

Session keys follow the same format as Telegram: `agentID/c{channelID}/{versionTS}`. Discord channel snowflake IDs (int64) are used as the chat identifier. Each channel/DM/thread has a unique channel ID.

Keys are cached in-memory and persisted to the session index for continuity across restarts. The first message from a user sets the agent's default channel (used for proactive messages like keepalive, cron, startup notifications).

---

## Differences from Telegram

| Aspect | Telegram | Discord |
|--------|----------|---------|
| Connection | HTTP long-polling per bot | Single WebSocket gateway |
| Message limit | 4096 chars | 2000 chars |
| Formatting | HTML (converted from Markdown) | Markdown (pass-through) |
| Streaming edit rate | 250ms default | 1200ms default |
| Facets | Separate bot tokens | Threads |
| Attachments | File ID → download URL | Direct CDN URL |
| Interactive UI | Inline keyboards | Message components (buttons) |
| Command prefix | `/` only | `.` and `/` |
| Bot per agent | One token per bot | Shared gateway, logical separation |

---

## Full Configuration Reference

See [CONFIG.md](CONFIG.md) for the complete field reference:
- [`[discord]`](CONFIG.md#discord) — global settings
- [`[agents.platforms.discord]`](CONFIG.md#platform-configuration-agentsplatformsdiscord) — per-agent overrides
