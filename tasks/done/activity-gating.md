# Task: Activity gating for CLI commands (#88)

## Problem

`clod send` and `clod branch` CLI commands fire regardless of whether anyone is actually using the agent. Heartbeats, cron jobs, and scheduled tasks burn mana on dead sessions where no human has talked in hours/days.

## Desired Behaviour

Both `clod send` and `clod branch` should gain an optional flag to skip execution if no real user activity in the last X hours.

Example:
```bash
# Only send heartbeat if user was active in last 8 hours
clod send -a clutch --if-active 8h "[heartbeat] ..."

# Only run branch if user was active in last 12 hours  
clod branch -a clutch --if-active 12h "do maintenance"
```

If the activity check fails (no recent activity), the command exits silently with exit code 0 (not an error — just skipped).

## CRITICAL: Defining "user activity"

**"User activity" must mean real Telegram messages from actual humans.** It must NOT count:
- Messages injected via `clod send` (heartbeats, cron sends)
- Messages injected via `clod branch` 
- System-injected messages (fork prompts, async results, watch notifications)
- Agent-to-agent messages (send_to_session)

If injected messages count as "activity", the gate defeats itself — every heartbeat send would reset the activity timer, making the gate useless.

## Implementation

This likely requires:
1. A separate "last real user message" timestamp per agent (or per session), distinct from general session `LastActivity`
2. The timestamp should only update when a message arrives via Telegram from an allowed user (the normal message path in telegram/bot.go)
3. Store this timestamp somewhere accessible to the CLI (state store, a file, etc.)
4. CLI reads the timestamp and compares against the `--if-active` duration

## Files to check

- CLI command handling (look for where `send` and `branch` subcommands are parsed)
- telegram/bot.go — where real user messages arrive
- State store or similar for persisting the timestamp

## When done: write/update tests, update SPEC.md, docs/CONFIG.md, docs/CLI.md (if it exists), commit with descriptive message, push.
