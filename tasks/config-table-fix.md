# Fix: /config table display bugs

## Problem A: Not splitting into separate messages
/config table should send separate messages — one for global config, one per agent. Currently sends one big concatenated response. Each section should be its own message so Telegram doesn't need to split mid-table.

## Problem B: HTML tags break on message split  
The output uses `<pre><code>` HTML tags. When Telegram splits a long message, the tags aren't re-added to continuation messages, so subsequent chunks render as garbage.

## Fix
1. Switch from HTML `<pre><code>` to markdown triple-backtick code blocks
2. Return multiple messages (one per section) instead of one big response. The command handler needs to support returning multiple responses, OR the /config table command should send each section via the bot directly.
3. Each section gets its own code block: "Global", "clutch", "fotini", etc.

## Docs
Update SPEC.md if the command output format is documented.
