# Multiball — Parallel Conversations

Multiball lets you fork an agent session into a parallel conversation on a second Telegram bot. Same agent, same context snapshot, different thread.

## Why

Single-threaded chat breaks down when you have multiple trains of thought. You're debugging a deploy and want to ask about something unrelated — but switching topics mid-conversation pollutes the context. Multiball gives you a second (or third) channel without losing the original thread.

## How It Works

```
You:    /multiball
Agent:  Forked to @wand_bot — same context, separate thread.
```

The fork creates a new session on a **secondary Telegram bot** (configured as a multiball bot). The forked session:

- Starts with the parent session's full conversation context
- Shares the same cached system prompt prefix (cheap fork)
- Runs independently from that point — messages in one don't appear in the other
- Has full tool access

### Bot Pool

Multiball bots are configured in `foci.toml` by name. Tokens are resolved from `secrets.toml` by convention: `"telegram.<botname>"`.

```toml
# Per-agent multiball bots:
[[agents]]
id = "myagent"
multiball_bots = ["wand", "crystal"]

# Or shared pool (available to all agents):
[telegram]
multiball_bots = ["wand", "crystal"]
```

With `secrets.toml`:
```toml
[telegram]
wand = "123456:ABC..."
crystal = "789012:DEF..."
```

Bots can be **per-agent** (dedicated) or **shared** (allocated from a pool). When all bots are in use, the fork request fails immediately.

### Session Lifecycle

- `/multiball` or `/mb` — fork from current session
- The forked session lives until it times out (configured via `multiball_session_ttl`, default 60m idle)
- When the TTL expires, the bot is reclaimed and returned to the pool
- Sessions survive service restarts (restored from disk)

## Agent-Side

From the agent's perspective, a multiball session is just another session. It has:

- Its own conversation history
- Its own compaction cycle
- Its own tool call context

The agent knows it's in a multiball session via its session key (e.g., `clutch/c123/1709590000/b1709123456`).

## Display Settings

Multiball sessions inherit the parent agent's display settings:

- `show_tool_calls` — whether tool calls are shown in Telegram
- `show_thinking` — whether thinking blocks are shown
- `display_width` — formatting width

These are applied when the session is forked and when restored after restart.

## Routing

All messages from a multiball session route through the correct bot — tool outputs, async notifications, spawn results, and `send_message_to_user` calls all go to the multiball bot's chat, not the primary bot.

## Use Cases

- **Parallel investigations**: debug in one thread, research in another
- **Context separation**: keep a long-running task clean while handling ad-hoc questions
- **Testing**: fork to test a risky operation without polluting the main session
