# Fix: Format-aware Telegram message splitting

## Problem
When a Telegram message is too long and gets split into multiple messages, any open formatting (code blocks, bold, italic, etc.) breaks at the split point. The continuation message has no opening tag, so it renders as plain text.

Most visible with `/config table` — the global config table is one large code block that exceeds Telegram's message limit.

## Requirements
1. When splitting a message that's inside a code block (``` or `<pre><code>`), close the block at the split point and reopen it at the start of the next chunk
2. Handle nested formatting if relevant (bold inside code block, etc.)
3. The split should happen at a line boundary when possible, not mid-line
4. This applies to ALL message splitting, not just /config — any long response with formatting should split correctly

## Approach
In the message chunking code (telegram/bot.go or wherever messages get split for Telegram's 4096-char limit):
- Track whether we're inside a code block (toggle on ``` or `<pre>`)
- When splitting: if inside a code block, append closing ``` (or `</pre></code>`) to current chunk and prepend opening to next chunk
- Same principle for other paired formatting if needed

## Docs
No doc changes needed — this is internal formatting behaviour.
