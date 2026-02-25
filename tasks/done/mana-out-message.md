# Task: Friendly Telegram message when mana runs out (#75)

## Problem

When mana (Anthropic rate limit) is exhausted, the user gets a raw API error. Should instead get a friendly Telegram message explaining what happened and when it will refill.

## Desired Behaviour

When an API call fails due to rate limiting (HTTP 429 or similar from Anthropic):
1. Don't show the raw error to the user
2. Send a friendly Telegram message like: "I've hit my rate limit. Mana refills on a 5-hour sliding window — should have capacity again in roughly X minutes."
3. If possible, estimate when capacity will return (based on the sliding window)

## Implementation Notes

- Check how Anthropic returns rate limit errors (HTTP 429, check response headers for retry-after or rate limit info)
- The agent loop / API call handler is where this should be caught
- The message should go to the user's Telegram chat, not just be logged
- Don't retry automatically — just inform the user

## Files to check

- Look at how API errors are currently handled in the agent loop
- Check anthropic/ package for error handling
- Check if Anthropic returns retry-after headers or rate limit metadata

## When done: write/update tests, update SPEC.md, docs/WIRING.md as needed, commit with descriptive message, push.
