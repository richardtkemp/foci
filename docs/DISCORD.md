# Discord Setup

## 1. Create a Discord Application

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
2. Click **New Application**, give it a name
3. Go to **Bot** in the left sidebar
4. Click **Reset Token** and copy the bot token
5. Under **Privileged Gateway Intents**, enable:
   - **Message Content Intent** (required)
   - **Server Members Intent** (optional)

## 2. Invite the Bot to Your Server

Go to **OAuth2 → URL Generator**:
- **Scopes:** `bot`, `applications.commands`
- **Bot Permissions:** `Send Messages`, `Send Messages in Threads`, `Manage Messages`, `Create Public Threads`, `Create Private Threads`, `Manage Threads`, `Embed Links`, `Attach Files`, `Read Message History`, `Use Slash Commands`, `Add Reactions`
- **Integration type:** `Guild Install`

Copy the generated URL and open it in your browser to invite the bot.

## 3. Get Your Discord User ID

Discord user IDs are numeric snowflakes (e.g. `651783976884895746`), not usernames. Using a username like `"my_name"` instead of the numeric ID will silently reject all messages.

To find your numeric ID:

1. Enable **Developer Mode** in Discord: Settings → Advanced → Developer Mode
2. Right-click your name in any chat or member list
3. Click **Copy User ID**

Alternatively, type `\@YourName` in any Discord chat — the escaped mention shows the numeric ID.

## 4. Configure Foci

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

The secret key matches the agent ID by default. If your agent is `[[agents]] id = "myagent"`, the token resolves from `discord.myagent` in secrets.

To use a different secret key:

```toml
[agents.platforms.discord]
bot = "mybot"                    # resolves from discord.mybot
bot_secret = "custom.secret.key" # or use a custom key entirely
```

### Interactive Setup (Alternative)

```bash
foci first-run
```

The setup wizard includes a Discord step that prompts for bot token and user ID. Auto-detect mode connects the bot and waits for you to send it a DM to capture your user ID.

---

## Commands

Type `.command` or `/command` in chat:

| Command | Effect |
|---------|--------|
| `.model haiku` | Switch model |
| `.status` | Show agent status |
| `.cost` | Show session cost |
| `.sessions` | List sessions |
| `/stop` | Cancel the current agent turn |
| `/done` | Detach a facet thread's bot, returning it to the pool |

Commands that return options (e.g. `.model`) render as interactive buttons.

---

## Streaming

Real-time token streaming is available:

```toml
[discord]
stream_output = true
stream_update_interval = "1200ms"  # default; Discord rate limits are strict
```

Per-agent override:

```toml
[agents.platforms.discord]
stream_output = true
stream_interval = "1500ms"
```

---

## Display Options

Control how tool calls and thinking are shown:

```toml
[discord]
show_tool_calls = "off"       # "off", "preview", or "full"
show_thinking = "off"         # "off", "compact", or "true"
display_width = 60
```

- **Tool calls:** `off` = silent, `preview` = shown then deleted, `full` = expandable summary with Show/Hide buttons
- **Thinking:** `off` = hidden, `compact` = behind a button, `true` = inline

Per-agent overrides go in `[agents.platforms.discord]`.

---

## Mention & Guild Filtering

In guild channels, the bot only responds to messages that `@mention` it (DMs are always processed):

```toml
[discord]
require_mention = true   # default

# Per-agent override:
[agents.platforms.discord]
require_mention = false  # respond to all messages
```

Lock the bot to a single server:

```toml
[discord]
guild_id = "123456789012345678"
```

---

## Facet Threads

Facets create Discord threads for branched conversations:

```toml
[discord]
auto_thread = true              # default
facet_session_ttl = "60m"       # idle time before thread is reclaimable
```

Use `/done` in a thread to detach the bot and return it to the pool.

---

## Attachments

Supported types:
- **Images** (JPEG, PNG, GIF, WebP)
- **PDFs**
- **Convertible documents** (text extraction)

Optionally save attachments to disk:

```toml
[discord]
received_files_dir = "/path/to/save"
```

---

## Full Configuration Reference

See [CONFIG.md](CONFIG.md) for the complete field reference.
