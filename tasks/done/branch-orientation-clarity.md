# Fix: Branch orientation message unclear about reply routing

## Problem
The current multiball orientation says:
> You have your own Telegram bot and CAN communicate with the user directly.

This is ambiguous. It doesn't explain that **plain replies are delivered to the user via Telegram** — i.e. the branch's Telegram bot IS how replies are sent. The agent interprets "CAN communicate directly" as meaning it needs to use `send_telegram` explicitly, leading to redundant tool calls (replying AND calling send_telegram).

## Fix
Make the orientation explicit: having your own Telegram bot means your replies go directly to the user. No need to use `send_telegram` unless you're sending files or proactive messages.

Something like:
> You have your own Telegram bot — your replies are sent directly to the user via Telegram.

The key point: "you have a Telegram bot" = "your replies reach the user." Don't leave that connection implicit.

## Scope
- `branch_orientation_prompt` or wherever the multiball orientation is templated
- Check other branch types (cron, spawn) for similar ambiguity
- Update SPEC.md if the orientation format is documented there

## Docs
Update SPEC.md and any relevant docs.
