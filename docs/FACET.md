# Facet — Parallel Conversations

Facet lets you fork an agent session into a parallel conversation on a second Telegram bot, a new app conversation, or (on delegated CC/opencode backends) a real transcript fork. Same agent, same context snapshot, different thread.

## Why

Single-threaded chat breaks down when you have multiple trains of thought. You're debugging a deploy and want to ask about something unrelated — but switching topics mid-conversation pollutes the context. Facet gives you a second (or third) channel without losing the original thread.

## How It Works

```
You:    /facet
Agent:  Forked to @wand_bot — same context, separate thread.
```

The fork creates a new session — either on a **secondary Telegram bot** (configured as a facet bot), as a **new app conversation** (surfacing in the app's session list), or, on delegated backends (CC/opencode), as a **real transcript fork** that copies the full conversation history into an independent session. The forked session:

- Starts with the parent session's full conversation context
- Shares the same cached system prompt prefix (cheap fork)
- Runs independently from that point — messages in one don't appear in the other
- Has full tool access

### Bot Pool

Facet bots are configured in `foci.toml` by name. Tokens are resolved from `secrets.toml` by convention: `"telegram.<botname>"`.

```toml
# Per-agent facet bots:
[[agents]]
id = "myagent"

  [[agents.platforms]]
  id = "telegram"
  facet_bots = ["wand", "crystal"]

# Or shared pool (available to all agents):
[[platforms]]
id = "telegram"
facet_bots = ["wand", "crystal"]
```

With `secrets.toml`:
```toml
[telegram]
wand = "123456:ABC..."
crystal = "789012:DEF..."
```

Bots can be **per-agent** (dedicated) or **shared** (allocated from a pool). When all bots are in use, the fork request fails immediately.

### Session Lifecycle

- `/facet` — fork from current session
- The forked session lives until it times out (configured via `facet_session_ttl`, default 60m idle)
- When the TTL expires, the bot is reclaimed and returned to the pool
- Sessions survive service restarts (restored from disk)

## Agent-Side

From the agent's perspective, a facet session is just another session. It has:

- Its own conversation history
- **No compaction by default** — facets default to `facet_no_compact = true` and do NOT compact unless explicitly opted in (`facet_no_compact = false`)
- Its own tool call context

The agent knows it's in a facet session via its session key (e.g., `clutch/c123/b1709123456`).

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `facet_bots` | string list | `[]` | Pool of secondary bot names available for facets (Telegram) |
| `facet_session_ttl` | duration | `"60m"` | Idle time before a facet session is reclaimed |
| `facet_no_compact` | bool | `true` | Whether facets skip compaction. Set `false` to enable compaction on facet sessions |

## Display Settings

Facet sessions inherit the parent agent's display settings:

- `show_tool_calls` — whether tool calls are shown in Telegram
- `show_thinking` — whether thinking blocks are shown
- `display_width` — formatting width

These are applied when the session is forked and when restored after restart.

## Routing

All messages from a facet session route through the correct bot — tool outputs, async notifications, spawn results, and `send_to_chat` calls all go to the facet bot's chat, not the primary bot.

## Use Cases

- **Parallel investigations**: debug in one thread, research in another
- **Context separation**: keep a long-running task clean while handling ad-hoc questions
- **Testing**: fork to test a risky operation without polluting the main session
