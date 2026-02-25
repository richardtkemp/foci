# Task: send_telegram should send to the originating session's chat, not default

## Problem

`send_telegram` always sends to the agent's primary bot's default chat (the last chat that messaged it). When an agent serves multiple users via per-chat sessions, this means messages go to the wrong person.

**Example:** Fotini agent has two users — Dick and Eleni. Eleni is the default chat. When a heartbeat or cron job runs on Dick's session and calls `send_telegram`, the message goes to Eleni instead of Dick.

## Current Behaviour

The `send_telegram` tool calls `bot.SendText()` which sends to `bot.chatID` — a single global chat ID on the primary bot. This is always the last user who messaged.

## Desired Behaviour

`send_telegram` should send to the chat that the current session belongs to. The session key contains the chat ID (format: `agent:NAME:chat:CHATID`). The tool should extract the chat ID from the session key and send to that specific chat.

## Implementation

- The tool needs access to the current session key (via context or deps)
- Extract chat ID from session key (parse `agent:X:chat:CHATID` format)
- Use `bot.client.SendMessage(chatID, ...)` with the extracted chat ID instead of `bot.SendText()` which uses the default
- Fallback: if session key doesn't contain a chat ID (e.g. spawn, cron branch), fall back to the default chat ID (existing behaviour)

## Files to check

- Look at how `send_telegram` is currently wired (tools/ and main.go)
- Check how session key is available in tool context
- Check `telegram/bot.go` for sending to a specific chat vs default

## When done: write/update tests, update SPEC.md, docs/WIRING.md, commit with descriptive message, push.
