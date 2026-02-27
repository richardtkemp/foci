# Task: Multiball send_telegram should use the multiball bot's Telegram

## Problem
When a multiball session uses the `send_telegram` tool, messages are sent via the main bot's Telegram account instead of the multiball bot's Telegram account. This is incorrect — multiball sessions should send from their own bot.

**This only applies to multiball branches (`/mb`, `/multiball`), NOT other branch types (heartbeats, cron, scheduled wakes, etc.).**

## Expected Behavior
- Multiball session sends Telegram message → uses the multiball bot token (e.g. `telegram.clutchling` or `telegram.clodbot`)
- Main session sends Telegram message → uses the main bot token (e.g. `telegram.clutch`)
- Other branches (heartbeats, cron) → use the main bot token (current behavior is correct)

## Investigation Points
1. How are multiball sessions created? Check `multiball.go` or wherever `/mb` is handled
2. How does `send_telegram` get its bot token? Is it from the agent config, session config, or hardcoded?
3. Multiball sessions likely need to know which Telegram bot they belong to — check if there's a `bot_token` field in the session or branch config
4. The multiball bot token should be configured somewhere — check `clod.toml` for multiball-related Telegram config

## Config Context
From secrets.toml, relevant tokens:
- `telegram.clutch` — main bot
- `telegram.clutchling` — per-agent multiball bot
- `telegram.clodbot` — shared multiball pool bot

## Important
- Don't change behavior for non-multiball branches
- Check how the Telegram sender is wired up — it may be a shared instance that needs to be overridden per-session
- Update tests, commit and push when done
